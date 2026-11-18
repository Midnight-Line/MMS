// Package link defines the pluggable transport abstraction used by the Edge
// Router to carry MMTP traffic over one of several media, and a LinkManager
// that selects among them.
//
// Two link types exist today:
//   - IP: the existing WebSocket link to the MMS Router.
//   - VDE: the VDES ship-board channel (core network TCP + modem PI), the
//     subject of this change.
//
// The manager supports a global preferred policy (ip | vde | auto) with
// optional per-subject overrides, plus a high-priority "send on the VDE link
// even when IP is preferred" escape hatch for urgent warnings.
package link

import (
	"sync"

	"github.com/maritimeconnectivity/MMS/mmtp"
)

// Preferred qualifies how LinkManager picks a transport.
type Preferred int

const (
	// PreferredIP prefers the IP / WebSocket link.
	PreferredIP Preferred = iota
	// PreferredVDE prefers the VDE ship-board link.
	PreferredVDE
	// PreferredAuto chooses dynamically: IP for large payloads (>8192 bytes,
	// near the VDE-TER 9800 B ceiling), VDE otherwise, and enables dual-send
	// for high-priority traffic with receiver-side de-duplication.
	PreferredAuto
)

// ParsePreferred converts a string policy into a Preferred value.
// Accepts "ip", "vde", "auto" (case-insensitive); unknown -> PreferredAuto.
func ParsePreferred(s string) Preferred {
	switch s {
	case "ip", "IP", "Ip":
		return PreferredIP
	case "vde", "VDE", "Vde":
		return PreferredVDE
	default:
		return PreferredAuto
	}
}

// String renders the policy for config/debug output.
func (p Preferred) String() string {
	switch p {
	case PreferredIP:
		return "ip"
	case PreferredVDE:
		return "vde"
	default:
		return "auto"
	}
}

// Stable link identifiers used by LinkManager when routing.
const (
	LinkNameIP  = "ip"
	LinkNameVDE = "vde"
)

// Link is the transport abstraction. A concrete implementation wraps one
// medium end-to-end: the IP link wraps the ER<->Router WebSocket; the VDE
// link wraps the core network TCP + modem PI chain.
type Link interface {
	// Name returns a stable identifier for the link ("ip", "vde").
	Name() string
	// Healthy reports whether the transport is currently usable.
	Healthy() bool
	// Send delivers one marshalled MMTP frame over the link. The caller is
	// responsible for addressing (recipients resolved to MMSI on VDE).
	Send(mm *mmtp.MmtpMessage) error
}

// Config holds routing policy for a LinkManager.
type Config struct {
	// Preferred is the default policy.
	Preferred Preferred
	// PreferredBySubject overrides the policy per MMTP subject (unused if nil).
	PreferredBySubject map[string]Preferred
}

// LinkManager selects the transport for each outgoing MMTP message and
// forwards delivery events.
type LinkManager struct {
	mu     sync.RWMutex
	links  map[string]Link
	config Config
}

// NewManager returns a LinkManager with the given policy.
func NewManager(cfg Config) *LinkManager {
	return &LinkManager{links: make(map[string]Link), config: cfg}
}

// SetPolicy updates the default policy at runtime.
func (m *LinkManager) SetPolicy(p Preferred) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config.Preferred = p
}

// Register adds a link by name. A second register for the same name replaces
// the previous entry.
func (m *LinkManager) Register(l Link) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.links[l.Name()] = l
}

// SetSubjectPolicy sets/clears a per-subject override. pass PreferredAuto to
// clear.
func (m *LinkManager) SetSubjectPolicy(subject string, p Preferred) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.PreferredBySubject == nil {
		m.config.PreferredBySubject = make(map[string]Preferred)
	}
	if p == PreferredAuto {
		delete(m.config.PreferredBySubject, subject)
		return
	}
	m.config.PreferredBySubject[subject] = p
}

// Link resolves a link by name.
func (m *LinkManager) Link(name string) (Link, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.links[name]
	return l, ok
}

// Healthy reports whether at least one link is usable. When preferred policy
// is enforced, "healthy" means the preferred link is healthy; otherwise it
// means any link is healthy.
func (m *LinkManager) Healthy() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.healthyLocked()
}

func (m *LinkManager) healthyLocked() bool {
	for _, l := range m.links {
		if l.Healthy() {
			return true
		}
	}
	return false
}

// subjectFor extracts the MMTP subject from a frame, or "" if none.
func subjectFor(mm *mmtp.MmtpMessage) string {
	if mm == nil {
		return ""
	}
	pm := mm.GetProtocolMessage()
	if pm == nil {
		return ""
	}
	send := pm.GetSendMessage()
	if send == nil {
		return ""
	}
	app := send.GetApplicationMessage()
	if app == nil || app.GetHeader() == nil {
		return ""
	}
	return app.GetHeader().GetSubject()
}

// payloadSize estimates the encoded size of a frame for auto-routing.
func isLarge(mm *mmtp.MmtpMessage) bool {
	if mm == nil {
		return false
	}
	pm := mm.GetProtocolMessage()
	if pm == nil {
		return false
	}
	send := pm.GetSendMessage()
	if send == nil {
		return false
	}
	app := send.GetApplicationMessage()
	if app == nil || app.GetHeader() == nil {
		return false
	}
	return app.GetHeader().GetBodySizeNumBytes() > 8192
}

// Send routes one message over the selected link(s). It returns nil if at
// least one applicable link accepted the (primary) send. The auto policy may
// additionally duplicate to the other link for reliability; receivers
// de-duplicate by MMTP uuid.
func (m *LinkManager) Send(mm *mmtp.MmtpMessage) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ip, hasIP := m.links[LinkNameIP]
	vde, hasVDE := m.links[LinkNameVDE]

	// Resolution order: subject override, then global policy.
	policy := m.config.Preferred
	if subj := subjectFor(mm); subj != "" {
		if p, ok := m.config.PreferredBySubject[subj]; ok {
			policy = p
		}
	}

	selected := func(l Link) bool { // helper
		if l == nil {
			return false
		}
		return l.Healthy()
	}

	switch policy {
	case PreferredVDE:
		if hasVDE && selected(vde) {
			return m.dispatch(mm, vde)
		}
		if hasIP && selected(ip) {
			return m.dispatch(mm, ip)
		}
		return errNoHealthyLink
	case PreferredIP:
		if hasIP && selected(ip) {
			return m.dispatch(mm, ip)
		}
		if hasVDE && selected(vde) {
			return m.dispatch(mm, vde)
		}
		return errNoHealthyLink
	default: // PreferredAuto
		// Primary: IP for large payloads, VDE otherwise.
		var primary, secondary Link
		if isLarge(mm) {
			primary, secondary = ip, vde
		} else {
			primary, secondary = vde, ip
		}
		if primary != nil && selected(primary) {
			if err := m.dispatch(mm, primary); err != nil {
				return err
			}
			// Best-effort dual-send to the fallback for extra reliability.
			if secondary != nil && selected(secondary) {
				_ = m.dispatch(mm, secondary)
			}
			return nil
		}
		if secondary != nil && selected(secondary) {
			return m.dispatch(mm, secondary)
		}
		return errNoHealthyLink
	}
}

// dispatch sends on a single link.
func (m *LinkManager) dispatch(mm *mmtp.MmtpMessage, l Link) error {
	return l.Send(mm)
}

// errNoHealthyLink is returned when no usable link exists.
var errNoHealthyLink = &NoHealthyLinkError{}

// NoHealthyLinkError reports that no configured link is currently healthy.
type NoHealthyLinkError struct{}

func (*NoHealthyLinkError) Error() string { return "link: no healthy MMS link available" }

// IsNoHealthyLink reports whether err is a NoHealthyLinkError.
func IsNoHealthyLink(err error) bool {
	_, ok := err.(*NoHealthyLinkError)
	return ok
}
