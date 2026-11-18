package main

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/vde/link"
	"github.com/maritimeconnectivity/MMS/vde/mapping"
	"github.com/maritimeconnectivity/MMS/vde/pi"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fakeShipModem stands in for a ship modem's PI TCP service.
type fakeShipModem struct {
	ln net.Listener

	mu       sync.Mutex
	conn     net.Conn
	received []string
}

func newFakeShipModem(t *testing.T) *fakeShipModem {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &fakeShipModem{ln: ln}
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			if line == "" {
				continue
			}
			f.mu.Lock()
			f.received = append(f.received, line)
			f.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeShipModem) addr() string { return f.ln.Addr().String() }

func (f *fakeShipModem) sentences() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.received))
	copy(out, f.received)
	return out
}

func (f *fakeShipModem) push(t *testing.T, sentence string) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.conn != nil
	}, 3*time.Second, 20*time.Millisecond, "modem client never connected")
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	_, err := conn.Write([]byte(sentence))
	require.NoError(t, err)
}

// TestModemLinkSendsThroughPI verifies that in ship mode an application message
// addressed to an MRN is transmitted as TDB sentences carrying the mapped MMSI,
// with a payload that round-trips back into the same MMTP frame.
func TestModemLinkSendsThroughPI(t *testing.T) {
	modem := newFakeShipModem(t)
	mp := mapping.New(0)
	mp.Upsert("urn:mrn:mcp:device:test:ship-a", 992359599, true)

	er := &EdgeRouter{outgoingChannel: make(chan *mmtp.MmtpMessage, 8)}
	l := newModemLink(modem.addr(), mp, er)
	l.Start()
	t.Cleanup(l.Stop)
	require.Eventually(t, l.Healthy, 3*time.Second, 20*time.Millisecond, "modem link never became healthy")

	appMsg := newTestAppMessage("urn:mrn:mcp:device:test:shore", []string{"urn:mrn:mcp:device:test:ship-a"})
	require.NoError(t, l.Send(newSendMessage(appMsg)))

	require.Eventually(t, func() bool { return len(modem.sentences()) == 1 }, 3*time.Second, 20*time.Millisecond)

	sentence, err := pi.Parse(modem.sentences()[0])
	require.NoError(t, err)
	require.Equal(t, "TDB", sentence.Formatter)
	tdb, err := pi.ParseTDB(sentence)
	require.NoError(t, err)
	require.EqualValues(t, 992359599, tdb.DestID)

	payload, err := pi.Decode6Bit(tdb.Data)
	require.NoError(t, err)
	decoded := &mmtp.MmtpMessage{}
	require.NoError(t, proto.Unmarshal(payload, decoded))
	require.Equal(t, appMsg.Body, decoded.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetBody())
}

// TestModemLinkUnmappedRecipientFailsPermanently mirrors the core network
// behaviour: an unknown recipient is a permanent failure.
func TestModemLinkUnmappedRecipientFailsPermanently(t *testing.T) {
	modem := newFakeShipModem(t)
	mp := mapping.New(0)

	er := &EdgeRouter{outgoingChannel: make(chan *mmtp.MmtpMessage, 8)}
	l := newModemLink(modem.addr(), mp, er)
	l.Start()
	t.Cleanup(l.Stop)
	require.Eventually(t, l.Healthy, 3*time.Second, 20*time.Millisecond)

	err := l.Send(newSendMessage(newTestAppMessage("urn:mrn:mcp:device:test:shore", []string{"urn:mrn:mcp:device:test:unknown"})))
	require.ErrorIs(t, err, ErrNoRecipientMapping)
	require.Empty(t, modem.sentences())
}

// TestModemLinkReceivesAndQueues verifies the ship-mode receive path: a payload
// received over the air is decoded, feeds the mapping table, and is queued for a
// locally connected subscriber.
func TestModemLinkReceivesAndQueues(t *testing.T) {
	modem := newFakeShipModem(t)
	mp := mapping.New(0)

	sub := NewSubscription("test.subject")
	agent := &Agent{agentUuid: uuid.NewString(), directMessages: true}
	agent.Mrn = "urn:mrn:mcp:device:test:shore"
	agent.Messages = make(map[string]*mmtp.MmtpMessage)
	agent.MsgMu = new(sync.RWMutex)
	agent.Notifications = make(map[string]*mmtp.MmtpMessage)
	agent.NotifyMu = new(sync.RWMutex)
	sub.AddSubscriber(agent)

	er := &EdgeRouter{
		outgoingChannel: make(chan *mmtp.MmtpMessage, 8),
		subscriptions:   map[string]*Subscription{"test.subject": sub},
		subMu:           new(sync.RWMutex),
		mrnToAgent:      map[string]*Agent{agent.Mrn: agent},
		mrnToAgentMu:    new(sync.RWMutex),
		agents:          map[string]*Agent{},
		agentsMu:        new(sync.RWMutex),
		awaitResponse:   map[string]*mmtp.MmtpMessage{},
		responseMu:      new(sync.RWMutex),
		wsMu:            new(sync.RWMutex),
	}

	l := newModemLink(modem.addr(), mp, er)
	l.Start()
	t.Cleanup(l.Stop)
	require.Eventually(t, l.Healthy, 3*time.Second, 20*time.Millisecond)

	sender := "urn:mrn:mcp:device:test:ship-b"
	frame := newSendMessage(newTestAppMessage(sender, nil))
	payload, err := proto.Marshal(frame)
	require.NoError(t, err)

	// The modem delivers the payload in an EDM followed by the ESI that carries
	// the sender's MMSI.
	modem.push(t, pi.Format(pi.SentenceStart, pi.TalkerVDES, "EDM", []string{"0", "", "992359598", pi.Encode6Bit(payload), "0"}))
	modem.push(t, pi.Format(pi.SentenceStart, pi.TalkerVDES, "ESI", []string{"0", "992359598", "7"}))

	require.Eventually(t, func() bool {
		agent.MsgMu.RLock()
		defer agent.MsgMu.RUnlock()
		return len(agent.Messages) == 1
	}, 3*time.Second, 20*time.Millisecond, "message was never queued for the subscriber")

	mmsi, ok := mp.MmsiFor(sender)
	require.True(t, ok, "the sender was not recorded in the MRN<->MMSI table")
	require.EqualValues(t, 992359598, mmsi)
}

// TestBuildVdeLinkSelectsMode verifies the mode switch wires up the right link.
func TestBuildVdeLinkSelectsMode(t *testing.T) {
	mp := mapping.New(0)
	er := &EdgeRouter{outgoingChannel: make(chan *mmtp.MmtpMessage, 8)}

	for _, mode := range []string{"shore", "core", ""} {
		l, stop, err := buildVdeLink(mode, "127.0.0.1:1", "127.0.0.1:2", mp, er)
		require.NoError(t, err, "mode %q", mode)
		require.IsType(t, &vdeLink{}, l, "mode %q should use the core network link", mode)
		require.Equal(t, link.LinkNameVDE, l.Name())
		stop()
	}

	for _, mode := range []string{"ship", "modem"} {
		l, stop, err := buildVdeLink(mode, "127.0.0.1:1", "127.0.0.1:2", mp, er)
		require.NoError(t, err, "mode %q", mode)
		require.IsType(t, &modemLink{}, l, "mode %q should use the modem link", mode)
		require.Equal(t, link.LinkNameVDE, l.Name())
		stop()
	}

	_, _, err := buildVdeLink("nonsense", "127.0.0.1:1", "127.0.0.1:2", mp, er)
	require.Error(t, err)
}
