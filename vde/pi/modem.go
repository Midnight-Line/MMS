package pi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// MaxFragmentBytes is the payload size carried by one TDB sentence, matching the
// 1400 six-bit characters defined for VDE long messages.
const MaxFragmentBytes = 1400

// MaxShortMessageBytes is the largest payload that fits in a single TSB sentence
// (the link ID 17 allocation of 215 bytes).
const MaxShortMessageBytes = 215

// Modem is a TCP client for the ship modem's PI (presentation interface). The
// modem emits sentences describing what it received over the air (EDM/ESI) and
// accepts sentences to transmit (TDB/TSB).
//
// Long messages travel as several TDB sentences chained through the internal
// message number (74 start, 75 continue, 76 end), which modems carry in the
// data type field; the Modem reassembles them before handing the payload to the
// caller.
type Modem struct {
	addr    string
	handler ModemHandler

	mu        sync.Mutex
	conn      net.Conn
	connected bool
	seq       int64
	pending   []byte // payload from an EDM still waiting for its ESI

	asm *Assembler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ModemHandler receives the decoded traffic coming from the modem.
type ModemHandler struct {
	// OnMessage is called with a complete payload received over the air. mmsi is
	// the peer MMSI reported by the modem, or 0 when it did not report one.
	OnMessage func(mmsi int64, payload []byte)
	// OnTransmitted is called when the modem reports that a message was sent.
	OnTransmitted func(destMMSI int64)
	// OnStatus reports transport up/down transitions.
	OnStatus func(connected bool, err error)
}

// NewModem returns a modem client for the given host:port.
func NewModem(addr string, handler ModemHandler) *Modem {
	return &Modem{addr: addr, handler: handler, asm: NewAssembler()}
}

// Addr returns the configured modem address.
func (m *Modem) Addr() string { return m.addr }

// Start launches the connection loop and returns immediately.
func (m *Modem) Start() {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx, m.cancel = ctx, cancel
	m.mu.Unlock()

	m.wg.Add(1)
	go m.runLoop(ctx)
}

// Stop terminates the connection loop and waits for it to finish.
func (m *Modem) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// Healthy reports whether the modem link is currently usable.
func (m *Modem) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// SendLong transmits one payload as a long message, fragmenting it into TDB
// sentences. destMMSI 0 means a broadcast.
func (m *Modem) SendLong(destMMSI int64, payload []byte, priority int64) error {
	fragments, err := SplitFragments(payload, MaxFragmentBytes)
	if err != nil {
		return err
	}
	total := int64(len(fragments))
	seq := m.nextSeq()
	for i, fragment := range fragments {
		tdb := TDB{
			TotalSentences: total,
			SentenceNumber: int64(i + 1),
			MessageSeq:     seq,
			DestID:         destMMSI,
			Priority:       priority,
			DataType:       int64(fragment.MsgNumber), // 74/75/76 chain the fragments
			Data:           Encode6Bit(fragment.Data),
			FillBits:       0,
		}
		if err := m.writeSentence(tdb.Sentence(TalkerVDES)); err != nil {
			return err
		}
	}
	return nil
}

// SendShort transmits one payload as a short message in a single TSB sentence.
// Broadcasts must use TSBUnacknowledged.
func (m *Modem) SendShort(destMMSI int64, payload []byte, priority, messageID int64) error {
	if len(payload) > MaxShortMessageBytes {
		return fmt.Errorf("pi: payload of %d bytes exceeds the %d byte short message limit", len(payload), MaxShortMessageBytes)
	}
	if destMMSI == 0 && messageID == TSBAcknowledged {
		return errors.New("pi: broadcasts must use the unacknowledged message ID")
	}
	tsb := TSB{
		TotalSentences: 1,
		SentenceNumber: 1,
		MessageSeq:     m.nextSeq(),
		DestID:         destMMSI,
		Priority:       priority,
		MessageID:      messageID,
		Data:           Encode6Bit(payload),
		FillBits:       0,
	}
	return m.writeSentence(tsb.Sentence(TalkerVDES))
}

func (m *Modem) nextSeq() int64 {
	m.mu.Lock()
	m.seq = (m.seq + 1) % 100 // message sequence numbers run 0..99
	seq := m.seq
	m.mu.Unlock()
	return seq
}

func (m *Modem) writeSentence(sentence string) error {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn == nil {
		return errors.New("pi: modem not connected")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, err := conn.Write([]byte(sentence))
	return err
}

func (m *Modem) runLoop(ctx context.Context) {
	defer m.wg.Done()
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := m.connectAndServe(ctx); err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				m.notifyStatus(false, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (m *Modem) connectAndServe(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.conn = conn
	m.connected = true
	m.mu.Unlock()
	m.notifyStatus(true, nil)

	defer func() {
		m.mu.Lock()
		if m.conn == conn {
			m.conn = nil
			m.connected = false
		}
		m.mu.Unlock()
		_ = conn.Close()
	}()

	// Unblock the scanner when the context is cancelled.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		m.handleLine(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("pi: modem closed the connection")
}

// handleLine parses one sentence and dispatches it. Unparsable input is ignored:
// modems interleave proprietary chatter that is not relevant here.
func (m *Modem) handleLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	sentence, err := Parse(line)
	if err != nil {
		return
	}

	switch sentence.Formatter {
	case "EDM":
		m.handleEDM(sentence)
	case "EDO":
		if edo, parseErr := ParseEDO(sentence); parseErr == nil && m.handler.OnTransmitted != nil {
			m.handler.OnTransmitted(edo.DestID)
		}
	case "TDB":
		m.handleTDB(sentence)
	case "TSB":
		m.handleTSB(sentence)
	case "ESI":
		m.handleESI(sentence)
	}
}

// handleEDM handles a received message. The standard puts the peer MMSI in the
// ESI sentence that follows the EDM, not in the EDM itself, so the payload is
// held until the ESI arrives. When the modem does populate the source field, the
// payload can be delivered immediately.
func (m *Modem) handleEDM(sentence Sentence) {
	edm, err := ParseEDM(sentence)
	if err != nil {
		return
	}
	payload, decodeErr := Decode6Bit(edm.Data)
	if decodeErr != nil {
		return
	}

	if edm.SourceID != 0 {
		m.deliver(edm.SourceID, payload)
		return
	}

	m.mu.Lock()
	stale := m.pending
	m.pending = payload
	m.mu.Unlock()

	// A previous EDM whose ESI never arrived would otherwise be lost.
	if stale != nil {
		m.deliver(0, stale)
	}
}

// handleESI picks up the peer MMSI of the preceding EDM/EDO sentence and
// releases any payload that was waiting for it.
func (m *Modem) handleESI(sentence Sentence) {
	esi, err := ParseESI(sentence)
	if err != nil {
		return
	}

	m.mu.Lock()
	payload := m.pending
	m.pending = nil
	m.mu.Unlock()

	if payload != nil {
		m.deliver(esi.MMSI, payload)
	}
}

func (m *Modem) deliver(mmsi int64, payload []byte) {
	if m.handler.OnMessage != nil {
		m.handler.OnMessage(mmsi, payload)
	}
}

// handleTDB reassembles long messages that the modem delivers fragment by
// fragment.
func (m *Modem) handleTDB(sentence Sentence) {
	tdb, err := ParseTDB(sentence)
	if err != nil {
		return
	}
	payload, decodeErr := Decode6Bit(tdb.Data)
	if decodeErr != nil {
		return
	}

	msgNumber := byte(tdb.DataType)
	switch msgNumber {
	case MsgStart, MsgContinue, MsgEnd:
		// Chained fragment: feed the assembler below.
	default:
		// No chain information: the modem delivered a self-contained message.
		if m.handler.OnMessage != nil {
			m.handler.OnMessage(tdb.SourceID, payload)
		}
		return
	}

	// A message that fits in a single sentence is transmitted as one Start
	// fragment with no End fragment following, so the assembler would wait
	// forever for a terminator that never arrives.
	if tdb.TotalSentences <= 1 {
		if m.handler.OnMessage != nil {
			m.handler.OnMessage(tdb.SourceID, payload)
		}
		return
	}

	conv := fmt.Sprintf("%d/%d", tdb.DestID, tdb.MessageSeq)
	assembled, complete := m.asm.Push(conv, Fragment{MsgNumber: msgNumber, Data: payload})
	if !complete {
		return
	}
	if m.handler.OnMessage != nil {
		m.handler.OnMessage(tdb.SourceID, assembled)
	}
}

// handleTSB treats a short message as a complete payload.
func (m *Modem) handleTSB(sentence Sentence) {
	tsb, err := ParseTSB(sentence)
	if err != nil {
		return
	}
	payload, decodeErr := Decode6Bit(tsb.Data)
	if decodeErr != nil {
		return
	}
	if m.handler.OnMessage != nil {
		m.handler.OnMessage(tsb.SourceID, payload)
	}
}

func (m *Modem) notifyStatus(connected bool, err error) {
	if m.handler.OnStatus != nil {
		m.handler.OnStatus(connected, err)
	}
}
