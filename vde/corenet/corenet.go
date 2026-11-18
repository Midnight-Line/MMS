package corenet

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/maritimeconnectivity/MMS/vde/mapping"
)

// Protocol constants for the VDE core network service interface.
//
// The core network is a TCP **server**; the on-board Edge Router ("business
// system") is the TCP **client**. Each message is a single UTF-8 JSON object
// terminated by '\n'. `data` is a raw VDE-PI payload base64-encoded on the
// wire. See docs/VDES核心网服务接口需求说明.docx.
const (
	TypeUplink   = "uplink"   // core -> business: {"type":"uplink","mmsi":<long>,"data":"<b64>"}
	TypeDownlink = "downlink" // business -> core: {"type":"downlink","id":"<seq>","mmsi":<long>,"data":"<b64>"}
	TypeAck      = "ack"      // core -> business: {"type":"ack","id":"...","status":"...","reason":"..."}
	TypeCoverage = "coverage" // core -> business (optional): {"type":"coverage","event":"enter|leave|timeout"}
	TypePing     = "ping"     // business -> core keepalive

	// Heartbeat tuning (mirrors the Java prototype).
	heartbeatIdle   = 30 * time.Second // send `ping` when idle this long
	connDeadline    = 90 * time.Second // fail a connection silent this long
	maxBackoff      = 30 * time.Second // reconnect backoff ceiling
	initialBackoff  = time.Second      // reconnect backoff floor
	readBufferBytes = 1 << 20          // PI payload can be large (VDE-TER ~9.8 KiB).
)

// Downlink is a request to the core network to send a payload to a vessel.
// MMSI == 0 means broadcast.
type Downlink struct {
	ID   string // client-generated sequence / message UUID
	MMSI int64  // target vessel; 0 = broadcast
	Data []byte // raw VDE-PI payload
}

// Ack is a delivery report for a previously submitted downlink. The core
// network may emit several acks for the same id over time; later ones
// supersede earlier ones with the same id.
type Ack struct {
	ID     string
	Status string // delivered | unreachable | expired | too_large | rejected
	Reason string
}

// Uplink is a message received by the core network from a vessel's radio and
// delivered to us. Data is a (possibly fragmented) PI payload originating at
// that MMSI.
type Uplink struct {
	MMSI int64
	Data []byte
}

// Coverage reports a vessel entering / leaving coverage.
type Coverage struct {
	Event string // enter | leave | timeout
}

// Handler is the set of callbacks the client invokes on inbound events.
type Handler struct {
	OnUplink   func(Uplink)
	OnAck      func(Ack)
	OnCoverage func(Coverage)
	OnStatus   func(connected bool, err error) // transport up/down transitions
}

// Client is a reconnecting TCP client for the VDE core network.
type Client struct {
	addr     string
	mapTable *mapping.Mapping // optional, for automatic uplink MMSI -> MRN feed
	h        Handler

	mu   sync.Mutex
	conn net.Conn
	seq  int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a core-network client that dials addr and dispatches events to
// h. If mp is non-nil, each uplink is also recorded there via ObserveUplink
// once the MRN is known (the caller supplies it in ProcessUplink); mapping is
// optional at construction time.
func New(addr string, h Handler, mp *mapping.Mapping) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		addr:     addr,
		h:        h,
		mapTable: mp,
		ctx:      ctx,
		cancel:   cancel,
		seq:      0,
	}
}

// Start launches the connection loop in the background. It does not block on
// the initial dial; the loop keeps retrying with backoff until ctx is done.
func (c *Client) Start() {
	c.wg.Add(1)
	go c.runLoop()
}

// Stop terminates the connection loop and closes the socket.
func (c *Client) Stop() {
	c.cancel()
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// Addr returns the target host:port.
func (c *Client) Addr() string { return c.addr }

// LookupMrn attempts to resolve an MMSI to a known MRN via the mapping table.
func (c *Client) LookupMrn(mmsi int64) (string, bool) {
	if c.mapTable == nil {
		return "", false
	}
	return c.mapTable.MrnFor(mmsi)
}

// Downlink sends one downlink request to the core network. id is generated
// from an incrementing sequence when empty. Safe to call concurrently.
func (c *Client) Downlink(mmsi int64, data []byte) (string, error) {
	id := c.nextID()
	msg := struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		MMSI int64  `json:"mmsi"`
		Data string `json:"data"`
	}{
		Type: TypeDownlink, ID: id, MMSI: mmsi,
		Data: base64.StdEncoding.EncodeToString(data),
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	if err := c.writeLine(append(line, '\n')); err != nil {
		return "", err
	}
	return id, nil
}

// Ping sends an explicit keepalive.
func (c *Client) Ping() error {
	return c.writeLine([]byte(`{"type":"ping"}` + "\n"))
}

func (c *Client) nextID() string {
	c.mu.Lock()
	c.seq++
	id := fmt.Sprintf("dl-%06d", c.seq)
	c.mu.Unlock()
	return id
}

// runLoop is the reconnection + read dispatch loop.
func (c *Client) runLoop() {
	defer c.wg.Done()
	backoff := initialBackoff
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		if err := c.connectAndServe(); err != nil {
			select {
			case <-c.ctx.Done():
				// Deliberate shutdown: do not report this as a link failure.
				return
			default:
				c.notifyStatus(false, err)
			}
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (c *Client) connectAndServe() error {
	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.notifyStatus(true, nil)
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.Close()
	}()

	// Heartbeat goroutine: ensure a connection idle too long is torn down so
	// runLoop reconnects, and poke the peer with ping while active.
	heartbeatDone := make(chan struct{})
	go c.heartbeat(conn, heartbeatDone)
	defer close(heartbeatDone)

	reader := bufio.NewReaderSize(conn, readBufferBytes)
	for {
		select {
		case <-c.ctx.Done():
			return context.Canceled
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(connDeadline))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			return err
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}
		c.handleLine(line)
	}
}

func (c *Client) handleLine(line []byte) {
	var raw struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return
	}
	switch raw.Type {
	case TypeUplink:
		var m struct {
			MMSI int64  `json:"mmsi"`
			Data string `json:"data"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		data, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			return
		}
		if c.h.OnUplink != nil {
			c.h.OnUplink(Uplink{MMSI: m.MMSI, Data: data})
		}
	case TypeAck:
		var m struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		if c.h.OnAck != nil {
			c.h.OnAck(Ack{ID: m.ID, Status: m.Status, Reason: m.Reason})
		}
	case TypeCoverage:
		var m struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		if c.h.OnCoverage != nil {
			c.h.OnCoverage(Coverage{Event: m.Event})
		}
	}
}

func (c *Client) heartbeat(conn net.Conn, done <-chan struct{}) {
	idle := time.NewTicker(heartbeatIdle)
	defer idle.Stop()
	for {
		select {
		case <-done:
			return
		case <-c.ctx.Done():
			return
		case <-idle.C:
			// Any write refreshes the read deadline implicitly handled in the
			// serve loop; a stuck connection will trip the 90s deadline there.
			_ = c.writeLine([]byte(`{"type":"ping"}` + "\n"))
		}
	}
}

func (c *Client) writeLine(line []byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("core network not connected")
	}
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := conn.Write(line)
	return err
}

func (c *Client) notifyStatus(connected bool, err error) {
	if c.h.OnStatus != nil {
		c.h.OnStatus(connected, err)
	}
}
