package mapping

import (
	"sync"
	"time"
)

// entry holds a single MRN<->MMSI binding.
type entry struct {
	mmsi          int64
	lastSeen      time.Time
	authoritative bool
}

// Mapping is a concurrency-safe local table translating MRN (the addressing
// subject of the Edge Router) into the MMSI that the VDE core network can
// address, and back.
//
// Two sources feed it (as agreed with the product team):
//  1. Uplink feed — every core-network `uplink` carries the source MMSI, and
//     the ship-side Edge Router publishes its own MRN. Binding the two keeps
//     the table fresh without an active discovery protocol.
//  2. Management-plane pre-provisioning — authoritative MRN<->MMSI pairs.
//
// Non-authoritative entries expire after ttl (default 15 min) so stale
// bindings do not misroute a vessel after a station change.
type Mapping struct {
	mu     sync.RWMutex
	mrnTo  map[string]*entry
	mmsiTo map[int64]string
	ttl    time.Duration
}

// New returns an empty mapping with the given freshness TTL.
func New(ttl time.Duration) *Mapping {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &Mapping{
		mrnTo:  make(map[string]*entry),
		mmsiTo: make(map[int64]string),
		ttl:    ttl,
	}
}

// Upsert binds an MRN<->MMSI pair. authoritative entries are never expired.
// If an existing non-authoritative entry is overwritten, the reverse index is
// kept consistent.
func (m *Mapping) Upsert(mrn string, mmsi int64, authoritative bool) {
	if mrn == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	old, existed := m.mrnTo[mrn]
	if existed && old.mmsi != mmsi {
		// The old MMSI is being reassigned away from this MRN; drop the
		// stale reverse entry if it still points back here.
		if cur, ok := m.mmsiTo[old.mmsi]; ok && cur == mrn {
			delete(m.mmsiTo, old.mmsi)
		}
	}

	// MRN<->MMSI is 1:1, so the reverse index always reflects the latest
	// binding claimed for mmsi.
	m.mmsiTo[mmsi] = mrn
	m.mrnTo[mrn] = &entry{mmsi: mmsi, lastSeen: now, authoritative: authoritative}
}

// ObserveUplink records a non-authoritative binding learned from an uplink
// observation (core network reported a message from mmsi sent by mrn).
func (m *Mapping) ObserveUplink(mrn string, mmsi int64) {
	m.Upsert(mrn, mmsi, false)
}

// MmsiFor resolves an MRN to an MMSI for downlink addressing, if known.
func (m *Mapping) MmsiFor(mrn string) (int64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.mrnTo[mrn]
	if !ok {
		return 0, false
	}
	return e.mmsi, true
}

// MrnFor resolves an MMSI to the most recently bound MRN (for uplink subject
// filtering and logging). Not guaranteed 1:1 if mis-configured.
func (m *Mapping) MrnFor(mmsi int64) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mrn, ok := m.mmsiTo[mmsi]
	return mrn, ok
}

// evictExpired removes non-authoritative entries past their TTL.
func (m *Mapping) evictExpired(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for mrn, e := range m.mrnTo {
		if e.authoritative {
			continue
		}
		if now.Sub(e.lastSeen) > m.ttl {
			delete(m.mrnTo, mrn)
			if m.mmsiTo[e.mmsi] == mrn {
				delete(m.mmsiTo, e.mmsi)
			}
		}
	}
}

// RunEviction periodically evicts expired entries until ctx is done.
func (m *Mapping) RunEviction(done <-chan struct{}, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			m.evictExpired(now)
		}
	}
}

// Snapshot returns a copy of the MRN->MMSI view (regardless of authoritativeness).
func (m *Mapping) Snapshot() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int64, len(m.mrnTo))
	for mrn, e := range m.mrnTo {
		out[mrn] = e.mmsi
	}
	return out
}
