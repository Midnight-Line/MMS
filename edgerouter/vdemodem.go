package main

import (
	"errors"
	"fmt"
	"sync"

	"github.com/charmbracelet/log"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/vde/link"
	"github.com/maritimeconnectivity/MMS/vde/mapping"
	"github.com/maritimeconnectivity/MMS/vde/pi"
	"google.golang.org/protobuf/proto"
)

// modemLink is the ship-side VDE transport. Instead of talking to the VDE core
// network directly it drives the ship modem over its PI (presentation
// interface), which performs the over-the-air framing and resource allocation.
//
// Both the shore-side core network link and this modem link satisfy the same
// link.Link contract, so the Edge Router runs unchanged on either end.
type modemLink struct {
	modem    *pi.Modem
	mapTable *mapping.Mapping
	er       *EdgeRouter

	mu        sync.RWMutex
	connected bool
}

func newModemLink(addr string, mp *mapping.Mapping, er *EdgeRouter) *modemLink {
	l := &modemLink{mapTable: mp, er: er}
	l.modem = pi.NewModem(addr, pi.ModemHandler{
		OnMessage: l.handleMessage,
		OnStatus: func(connected bool, err error) {
			l.mu.Lock()
			l.connected = connected
			l.mu.Unlock()
			if err != nil {
				log.Warn("VDE ship modem link down:", err)
			} else if connected {
				log.Info("VDE ship modem link up")
			}
		},
	})
	return l
}

func (l *modemLink) Name() string { return link.LinkNameVDE }

func (l *modemLink) Healthy() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.connected
}

// Send hands the MMTP frame to the modem, which fragments and transmits it. The
// 6-bit encoding and TDB framing happen inside the modem client.
func (l *modemLink) Send(mm *mmtp.MmtpMessage) error {
	data, err := proto.Marshal(mm)
	if err != nil {
		return fmt.Errorf("could not marshal MMTP message for the VDE modem: %w", err)
	}

	header := mm.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader()
	if header == nil {
		return errors.New("MMTP message carries no application message header")
	}

	// Priority 2 (highest) for alert-style traffic, 0 otherwise; the modem
	// currently ignores this but the field is part of the standard.
	var priority int64
	if header.GetQosProfile() == "alert" {
		priority = 2
	}

	targets, err := l.targetsFor(header)
	if err != nil {
		return err
	}

	var firstErr error
	for _, mmsi := range targets {
		if sendErr := l.modem.SendLong(mmsi, data, priority); sendErr != nil {
			log.Warnf("Could not transmit message over the VDE modem to MMSI %d: %v", mmsi, sendErr)
			if firstErr == nil {
				firstErr = sendErr
			}
			continue
		}
		log.Debugf("Transmitted message over the VDE modem to MMSI %d (%d bytes)", mmsi, len(data))
	}
	return firstErr
}

func (l *modemLink) targetsFor(header *mmtp.ApplicationMessageHeader) ([]int64, error) {
	recipients := header.GetRecipients().GetRecipients()
	if len(recipients) == 0 {
		// Subject addressed: broadcast inside the current coverage area.
		return []int64{0}, nil
	}

	targets := make([]int64, 0, len(recipients))
	for _, recipient := range recipients {
		mmsi, ok := l.mapTable.MmsiFor(recipient)
		if !ok {
			log.Warnf("No MMSI known for recipient %s, skipping VDE transmission", recipient)
			continue
		}
		targets = append(targets, mmsi)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w (%d recipients)", ErrNoRecipientMapping, len(recipients))
	}
	return targets, nil
}

// Start begins the modem connection loop.
func (l *modemLink) Start() { l.modem.Start() }

// Stop terminates the modem connection loop.
func (l *modemLink) Stop() { l.modem.Stop() }

// buildVdeLink constructs the VDE link for the configured attachment mode and
// starts its connection loop. The returned stop function shuts it down.
//
//   - shore/core: the ER sits behind the VDE core network and hands whole MMTP
//     frames to it; the core network performs the VDE framing.
//   - ship: the ER drives the ship modem over PI, which performs framing and
//     over-the-air resource handling.
func buildVdeLink(mode, coreAddr, modemAddr string, mp *mapping.Mapping, er *EdgeRouter) (link.Link, func(), error) {
	switch mode {
	case "shore", "core", "":
		l := newVdeLink(coreAddr, mp, er)
		l.client.Start()
		return l, l.client.Stop, nil
	case "ship", "modem":
		l := newModemLink(modemAddr, mp, er)
		l.Start()
		return l, l.Stop, nil
	default:
		return nil, nil, fmt.Errorf("unknown VDE mode %q (expected shore, core or ship)", mode)
	}
}

// handleMessage decodes a payload received over the air and feeds it into the
// same distribution path used by the IP link.
func (l *modemLink) handleMessage(mmsi int64, payload []byte) {
	frame := &mmtp.MmtpMessage{}
	if err := proto.Unmarshal(payload, frame); err != nil {
		log.Warnf("Could not decode MMTP frame received over VDE from MMSI %d: %v", mmsi, err)
		return
	}

	appMsg := frame.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
	if appMsg == nil {
		log.Warnf("Ignoring non-application MMTP frame received over VDE from MMSI %d", mmsi)
		return
	}

	if sender := appMsg.GetHeader().GetSender(); sender != "" && mmsi != 0 {
		l.mapTable.ObserveUplink(sender, mmsi)
	}

	l.er.queueApplicationMessage(appMsg)
}
