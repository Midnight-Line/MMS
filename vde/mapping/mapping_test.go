package mapping

import (
	"testing"
	"time"
)

func TestUpsertAndResolve(t *testing.T) {
	m := New(15 * time.Minute)
	m.Upsert("urn:mrn:ship:a", 123456789, false)

	if mmsi, ok := m.MmsiFor("urn:mrn:ship:a"); !ok || mmsi != 123456789 {
		t.Fatalf("expected MMSI 123456789, got %d ok=%v", mmsi, ok)
	}
	if mrn, ok := m.MrnFor(123456789); !ok || mrn != "urn:mrn:ship:a" {
		t.Fatalf("expected MRN back, got %q ok=%v", mrn, ok)
	}
}

func TestExpiry(t *testing.T) {
	m := New(100 * time.Millisecond)
	m.ObserveUplink("urn:mrn:ship:b", 999)
	time.Sleep(200 * time.Millisecond)
	m.evictExpired(time.Now())
	if _, ok := m.MmsiFor("urn:mrn:ship:b"); ok {
		t.Fatal("non-authoritative entry should have expired")
	}
}

func TestAuthoritativeNotExpired(t *testing.T) {
	m := New(50 * time.Millisecond)
	m.Upsert("urn:mrn:ship:c", 777, true)
	time.Sleep(100 * time.Millisecond)
	m.evictExpired(time.Now())
	if _, ok := m.MmsiFor("urn:mrn:ship:c"); !ok {
		t.Fatal("authoritative entry must not expire")
	}
}
