package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"github.com/maritimeconnectivity/MMS/vde/corenet"
	"github.com/maritimeconnectivity/MMS/vde/link"
	"github.com/maritimeconnectivity/MMS/vde/mapping"
	"google.golang.org/protobuf/proto"
)

// duplicateWindow is how long the fingerprint of an application message is
// remembered, so that the same message arriving over both the IP and the VDE
// transport is only delivered once.
const duplicateWindow = 5 * time.Minute

// ErrNoRecipientMapping signals a permanent delivery failure: none of the
// addressed recipients has a known MMSI, so retrying cannot help.
var ErrNoRecipientMapping = errors.New("no MMSI mapping known for any recipient")

// ipLink adapts the existing WebSocket connection towards the MMS Router to the
// generic link.Link interface. It carries all protocol traffic (CONNECT,
// SUBSCRIBE, NOTIFY, RECEIVE, ...) and any application message that is routed
// over IP.
type ipLink struct {
	er *EdgeRouter
}

func (l *ipLink) Name() string { return link.LinkNameIP }

// Healthy reports whether the WebSocket is currently usable. A TryRLock is used
// because the write mutex is held for the whole duration of a reconnect
// attempt; blocking here would stall the outgoing message loop.
func (l *ipLink) Healthy() bool {
	if !l.er.wsMu.TryRLock() {
		return false
	}
	defer l.er.wsMu.RUnlock()
	return l.er.routerWs != nil
}

func (l *ipLink) Send(mm *mmtp.MmtpMessage) error {
	if !l.er.wsMu.TryRLock() {
		return errors.New("websocket is reconnecting")
	}
	conn := l.er.routerWs
	l.er.wsMu.RUnlock()
	if conn == nil {
		return errors.New("no websocket connection to the MMS Router")
	}
	return rw.WriteMessage(context.Background(), conn, mm)
}

// vdeLink adapts the VDE core network TCP connection to the generic link.Link
// interface. It serialises MMTP frames to protobuf, hands them to the core
// network, and reassembles/decodes uplink traffic back into MMTP frames.
type vdeLink struct {
	client   *corenet.Client
	mapTable *mapping.Mapping
	er       *EdgeRouter

	mu        sync.RWMutex
	connected bool
}

func newVdeLink(addr string, mp *mapping.Mapping, er *EdgeRouter) *vdeLink {
	l := &vdeLink{mapTable: mp, er: er}
	l.client = corenet.New(addr, corenet.Handler{
		OnUplink:   l.handleUplink,
		OnAck:      l.handleAck,
		OnCoverage: l.handleCoverage,
		OnStatus: func(connected bool, err error) {
			l.mu.Lock()
			l.connected = connected
			l.mu.Unlock()
			if err != nil {
				log.Warn("VDE core network link down:", err)
			} else if connected {
				log.Info("VDE core network link up")
			}
		},
	}, mp)
	return l
}

func (l *vdeLink) Name() string { return link.LinkNameVDE }

func (l *vdeLink) Healthy() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.connected
}

// Send marshals the MMTP frame and hands it to the VDE core network. Framing
// into VDE PI sentences (including 6-bit ASCII encoding and fragmentation) is
// the responsibility of the core network / modem, so the payload here is the
// raw protobuf encoded MMTP message.
func (l *vdeLink) Send(mm *mmtp.MmtpMessage) error {
	data, err := proto.Marshal(mm)
	if err != nil {
		return fmt.Errorf("could not marshal MMTP message for VDE: %w", err)
	}

	targets, err := l.targetsFor(mm)
	if err != nil {
		return err
	}

	var firstErr error
	for _, mmsi := range targets {
		if _, err := l.client.Downlink(mmsi, data); err != nil {
			log.Warnf("Could not hand message to VDE core network for MMSI %d: %v", mmsi, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		log.Debugf("Handed message to VDE core network for MMSI %d (%d bytes)", mmsi, len(data))
	}
	return firstErr
}

// targetsFor resolves the MMTP addressing (recipients or subject) into the MMSI
// values that the core network needs for downlink delivery. MMSI 0 means a
// broadcast to all ships inside the current coverage area.
func (l *vdeLink) targetsFor(mm *mmtp.MmtpMessage) ([]int64, error) {
	header := mm.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader()
	if header == nil {
		return nil, errors.New("MMTP message carries no application message header")
	}

	recipients := header.GetRecipients().GetRecipients()
	if len(recipients) == 0 {
		// Subject addressed (or malformed): deliver as a coverage-area broadcast.
		return []int64{0}, nil
	}

	targets := make([]int64, 0, len(recipients))
	for _, recipient := range recipients {
		mmsi, ok := l.mapTable.MmsiFor(recipient)
		if !ok {
			log.Warnf("No MMSI known for recipient %s, skipping VDE delivery", recipient)
			continue
		}
		targets = append(targets, mmsi)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w (%d recipients)", ErrNoRecipientMapping, len(recipients))
	}
	return targets, nil
}

func (l *vdeLink) handleUplink(uplink corenet.Uplink) {
	frame := &mmtp.MmtpMessage{}
	if err := proto.Unmarshal(uplink.Data, frame); err != nil {
		log.Warnf("Could not decode MMTP frame received over VDE from MMSI %d: %v", uplink.MMSI, err)
		return
	}

	appMsg := frame.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
	if appMsg == nil {
		log.Warnf("Ignoring non-application MMTP frame received over VDE from MMSI %d", uplink.MMSI)
		return
	}

	// Feed the MRN <-> MMSI table with what the sender claims to be.
	if sender := appMsg.GetHeader().GetSender(); sender != "" {
		l.mapTable.ObserveUplink(sender, uplink.MMSI)
	}

	// Nothing subscribed locally means the message has to continue into the
	// MMS Router network when this Edge Router acts as a VDE-TER gateway.
	queued, handled := l.er.queueApplicationMessage(appMsg)
	if queued == 0 && !handled {
		l.er.relayToRouter(appMsg)
	}
}

func (l *vdeLink) handleAck(ack corenet.Ack) {
	switch ack.Status {
	case "delivered", "sent", "received":
		log.Debugf("VDE delivery %s for %s", ack.Status, ack.ID)
	default:
		log.Warnf("VDE delivery %s for %s: %s", ack.Status, ack.ID, ack.Reason)
	}
}

func (l *vdeLink) handleCoverage(coverage corenet.Coverage) {
	log.Infof("VDE coverage event: %s", coverage.Event)
}

// queueApplicationMessage distributes an application message to locally
// connected agents, honouring subject subscriptions and direct recipients.
//
// It returns how many local agents the message was queued for, which lets a
// gateway Edge Router decide whether the message still has to be relayed to
// another transport, together with a flag telling whether the message had
// already been handled. The same application message can reach an Edge Router
// twice when it is subscribed to a subject on the MMS Router while the VDE-TER
// gateway relays the same subject over the air interface, so duplicates are
// recognised on their content and dropped.
func (er *EdgeRouter) queueApplicationMessage(appMsg *mmtp.ApplicationMessage) (int, bool) {
	now := time.Now()
	msgExpires := appMsg.GetHeader().GetExpires()
	if now.Unix() > msgExpires || msgExpires > now.Add(ExpirationLimit).Unix() {
		log.Debug("Discarding expired application message")
		return 0, true
	}

	if er.seenMessage(appMsg, now) {
		log.Debugf("Discarding duplicate application message from %s", appMsg.GetHeader().GetSender())
		return 0, true
	}

	incomingMessage := &mmtp.MmtpMessage{
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

	queued := 0
	switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
	case *mmtp.ApplicationMessageHeader_Subject:
		er.subMu.RLock()
		subscription := er.subscriptions[subjectOrRecipient.Subject]
		var subscribers []*Agent
		if subscription != nil {
			subscribers = subscription.SnapshotSubscribers()
		}
		er.subMu.RUnlock()
		for _, subscriber := range subscribers {
			if err := subscriber.QueueMessage(incomingMessage); err != nil {
				log.Error("Could not queue message:", err)
				continue
			}
			queued++
		}
	case *mmtp.ApplicationMessageHeader_Recipients:
		er.mrnToAgentMu.RLock()
		for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
			agent, ok := er.mrnToAgent[recipient]
			if ok && agent.directMessages {
				if err := agent.QueueMessage(incomingMessage); err != nil {
					log.Error("Could not queue message for Agent:", err)
					continue
				}
				queued++
			}
		}
		er.mrnToAgentMu.RUnlock()
	}
	return queued, false
}

// seenMessage records the content of an application message and reports
// whether it had been seen before. Messages are identified by the same fields
// that are covered by the message signature, so the same payload arriving over
// IP and over VDE maps to the same fingerprint even though every hop gives the
// carrying MMTP message a fresh UUID.
func (er *EdgeRouter) seenMessage(appMsg *mmtp.ApplicationMessage, now time.Time) bool {
	header := appMsg.GetHeader()
	digest := sha256.New()
	digest.Write([]byte(header.GetSubject()))
	for _, recipient := range header.GetRecipients().GetRecipients() {
		digest.Write([]byte(recipient))
	}
	digest.Write([]byte(strconv.FormatInt(header.GetExpires(), 10)))
	digest.Write([]byte(header.GetSender()))
	digest.Write([]byte(strconv.Itoa(int(header.GetBodySizeNumBytes()))))
	digest.Write(appMsg.GetBody())
	key := string(digest.Sum(nil))

	er.seenMu.Lock()
	defer er.seenMu.Unlock()
	if er.seen == nil {
		er.seen = map[string]time.Time{}
	}
	if seenAt, ok := er.seen[key]; ok && now.Sub(seenAt) < duplicateWindow {
		return true
	}
	er.seen[key] = now
	for seenKey, seenAt := range er.seen {
		if now.Sub(seenAt) >= duplicateWindow {
			delete(er.seen, seenKey)
		}
	}
	return false
}
