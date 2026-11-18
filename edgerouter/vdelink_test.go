package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/vde/mapping"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fakeCoreNet is a minimal stand-in for the VDE core network TCP service. It
// records every downlink line it receives and can push uplink lines back.
type fakeCoreNet struct {
	ln   net.Listener
	mu   sync.Mutex
	down []map[string]any
	conn net.Conn
}

func newFakeCoreNet(t *testing.T) *fakeCoreNet {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &fakeCoreNet{ln: ln}
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			var m map[string]any
			if json.Unmarshal(scanner.Bytes(), &m) != nil {
				continue
			}
			if m["type"] == "ping" {
				continue
			}
			f.mu.Lock()
			f.down = append(f.down, m)
			f.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeCoreNet) addr() string { return f.ln.Addr().String() }

func (f *fakeCoreNet) downlinks() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.down))
	copy(out, f.down)
	return out
}

// pushUplink sends an uplink JSON line to the connected business system.
func (f *fakeCoreNet) pushUplink(t *testing.T, mmsi int64, payload []byte) {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "uplink",
		"mmsi": mmsi,
		"data": base64.StdEncoding.EncodeToString(payload),
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.conn != nil
	}, 2*time.Second, 10*time.Millisecond, "core network never connected")
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	_, err = conn.Write(append(line, '\n'))
	require.NoError(t, err)
}

func newTestAppMessage(sender string, recipients []string) *mmtp.ApplicationMessage {
	header := &mmtp.ApplicationMessageHeader{
		Expires:          time.Now().Add(time.Minute).Unix(),
		Sender:           sender,
		BodySizeNumBytes: 4,
	}
	if len(recipients) > 0 {
		header.SubjectOrRecipient = &mmtp.ApplicationMessageHeader_Recipients{
			Recipients: &mmtp.Recipients{Recipients: recipients},
		}
	} else {
		header.SubjectOrRecipient = &mmtp.ApplicationMessageHeader_Subject{Subject: "test.subject"}
	}
	return &mmtp.ApplicationMessage{Header: header, Body: []byte("body")}
}

func newSendMessage(appMsg *mmtp.ApplicationMessage) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SEND_MESSAGE,
				Body: &mmtp.ProtocolMessage_SendMessage{
					SendMessage: &mmtp.Send{ApplicationMessage: appMsg},
				},
			},
		},
	}
}

// TestVdeLinkSendsDownlinkWithResolvedMmsi verifies that an application message
// addressed to an MRN is handed to the core network with the mapped MMSI and a
// payload that round-trips back into the same MMTP frame.
func TestVdeLinkSendsDownlinkWithResolvedMmsi(t *testing.T) {
	core := newFakeCoreNet(t)
	mp := mapping.New(0)
	mp.Upsert("urn:mrn:mcp:device:test:ship-a", 992359599, true)

	er := &EdgeRouter{outgoingChannel: make(chan *mmtp.MmtpMessage, 8)}
	l := newVdeLink(core.addr(), mp, er)
	l.client.Start()
	t.Cleanup(l.client.Stop)

	require.Eventually(t, l.Healthy, 3*time.Second, 20*time.Millisecond, "VDE link never became healthy")

	appMsg := newTestAppMessage("urn:mrn:mcp:device:test:sender", []string{"urn:mrn:mcp:device:test:ship-a"})
	require.NoError(t, l.Send(newSendMessage(appMsg)))

	require.Eventually(t, func() bool { return len(core.downlinks()) == 1 }, 3*time.Second, 20*time.Millisecond)
	down := core.downlinks()[0]
	require.Equal(t, "downlink", down["type"])
	require.EqualValues(t, 992359599, down["mmsi"])
	require.NotEmpty(t, down["id"])

	raw, err := base64.StdEncoding.DecodeString(down["data"].(string))
	require.NoError(t, err)
	decoded := &mmtp.MmtpMessage{}
	require.NoError(t, proto.Unmarshal(raw, decoded))
	require.Equal(t, appMsg.Body, decoded.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetBody())
	require.Equal(t,
		appMsg.GetHeader().GetRecipients().GetRecipients(),
		decoded.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader().GetRecipients().GetRecipients())
}

// TestVdeLinkSubjectAddressedBecomesBroadcast verifies that subject-addressed
// traffic (no MRN recipients) is delivered as a coverage-area broadcast.
func TestVdeLinkSubjectAddressedBecomesBroadcast(t *testing.T) {
	core := newFakeCoreNet(t)
	mp := mapping.New(0)

	er := &EdgeRouter{outgoingChannel: make(chan *mmtp.MmtpMessage, 8)}
	l := newVdeLink(core.addr(), mp, er)
	l.client.Start()
	t.Cleanup(l.client.Stop)
	require.Eventually(t, l.Healthy, 3*time.Second, 20*time.Millisecond)

	require.NoError(t, l.Send(newSendMessage(newTestAppMessage("urn:mrn:mcp:device:test:sender", nil))))

	require.Eventually(t, func() bool { return len(core.downlinks()) == 1 }, 3*time.Second, 20*time.Millisecond)
	require.EqualValues(t, 0, core.downlinks()[0]["mmsi"])
}

// TestVdeLinkUnmappedRecipientFailsPermanently verifies that an unknown
// recipient produces ErrNoRecipientMapping so the caller can drop the message
// instead of retrying forever.
func TestVdeLinkUnmappedRecipientFailsPermanently(t *testing.T) {
	core := newFakeCoreNet(t)
	mp := mapping.New(0)

	er := &EdgeRouter{outgoingChannel: make(chan *mmtp.MmtpMessage, 8)}
	l := newVdeLink(core.addr(), mp, er)
	l.client.Start()
	t.Cleanup(l.client.Stop)
	require.Eventually(t, l.Healthy, 3*time.Second, 20*time.Millisecond)

	err := l.Send(newSendMessage(newTestAppMessage("urn:mrn:mcp:device:test:sender", []string{"urn:mrn:mcp:device:test:unknown"})))
	require.ErrorIs(t, err, ErrNoRecipientMapping)
	require.Empty(t, core.downlinks(), "nothing should be handed to the core network")
}

// TestVdeUplinkIsDecodedAndQueuedForSubscriber verifies the receive path: an
// uplink MMTP frame is decoded, feeds the MRN<->MMSI table, and is queued for a
// locally connected subscriber.
func TestVdeUplinkIsDecodedAndQueuedForSubscriber(t *testing.T) {
	core := newFakeCoreNet(t)
	mp := mapping.New(0)

	subs := make(map[string]*Subscription)
	subMu := new(sync.RWMutex)
	sub := NewSubscription("test.subject")
	agent := &Agent{agentUuid: uuid.NewString(), directMessages: true}
	agent.Mrn = "urn:mrn:mcp:device:test:shore"
	agent.Messages = make(map[string]*mmtp.MmtpMessage)
	agent.MsgMu = new(sync.RWMutex)
	agent.Notifications = make(map[string]*mmtp.MmtpMessage)
	agent.NotifyMu = new(sync.RWMutex)
	sub.AddSubscriber(agent)
	subs["test.subject"] = sub

	er := &EdgeRouter{
		outgoingChannel: make(chan *mmtp.MmtpMessage, 8),
		subscriptions:   subs,
		subMu:           subMu,
		mrnToAgent:      map[string]*Agent{agent.Mrn: agent},
		mrnToAgentMu:    new(sync.RWMutex),
		agents:          map[string]*Agent{},
		agentsMu:        new(sync.RWMutex),
		awaitResponse:   map[string]*mmtp.MmtpMessage{},
		responseMu:      new(sync.RWMutex),
		wsMu:            new(sync.RWMutex),
	}

	l := newVdeLink(core.addr(), mp, er)
	l.client.Start()
	t.Cleanup(l.client.Stop)
	require.Eventually(t, l.Healthy, 3*time.Second, 20*time.Millisecond)

	sender := "urn:mrn:mcp:device:test:ship-b"
	frame := newSendMessage(newTestAppMessage(sender, nil))
	payload, err := proto.Marshal(frame)
	require.NoError(t, err)
	core.pushUplink(t, 992359598, payload)

	require.Eventually(t, func() bool {
		agent.MsgMu.RLock()
		defer agent.MsgMu.RUnlock()
		return len(agent.Messages) == 1
	}, 3*time.Second, 20*time.Millisecond, "uplink was never queued for the subscriber")

	// The uplink must have taught the mapping table who this MMSI is.
	mmsi, ok := mp.MmsiFor(sender)
	require.True(t, ok, "uplink sender was not recorded in the MRN<->MMSI table")
	require.EqualValues(t, 992359598, mmsi)
}
