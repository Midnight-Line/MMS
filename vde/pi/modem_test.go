package pi

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModem is a stand-in for a ship modem's PI TCP service. It records the
// sentences it receives and can push sentences back to the client.
type fakeModem struct {
	ln net.Listener

	mu       sync.Mutex
	conn     net.Conn
	received []string
}

func newFakeModem(t *testing.T) *fakeModem {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	f := &fakeModem{ln: ln}
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

func (f *fakeModem) addr() string { return f.ln.Addr().String() }

func (f *fakeModem) sentences() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.received))
	copy(out, f.received)
	return out
}

// push sends one sentence to the connected client.
func (f *fakeModem) push(t *testing.T, sentence string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		conn := f.conn
		f.mu.Unlock()
		if conn != nil {
			if _, err := conn.Write([]byte(sentence)); err != nil {
				t.Fatalf("push failed: %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("modem client never connected")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestModemSendLongFragmentsPayload(t *testing.T) {
	fake := newFakeModem(t)
	m := NewModem(fake.addr(), ModemHandler{})
	m.Start()
	t.Cleanup(m.Stop)
	waitFor(t, "modem link up", m.Healthy)

	// Two fragments: one full fragment plus a remainder.
	payload := make([]byte, MaxFragmentBytes+100)
	for i := range payload {
		payload[i] = byte('A' + i%26)
	}
	if err := m.SendLong(992359599, payload, 2); err != nil {
		t.Fatalf("SendLong failed: %v", err)
	}

	waitFor(t, "two TDB sentences", func() bool { return len(fake.sentences()) == 2 })
	sentences := fake.sentences()

	var rebuilt []byte
	for i, line := range sentences {
		s, err := Parse(line)
		if err != nil {
			t.Fatalf("sentence %d unparsable (%q): %v", i, line, err)
		}
		if s.Formatter != "TDB" {
			t.Fatalf("sentence %d has formatter %q, want TDB", i, s.Formatter)
		}
		tdb, err := ParseTDB(s)
		if err != nil {
			t.Fatalf("sentence %d not a TDB: %v", i, err)
		}
		if tdb.TotalSentences != 2 {
			t.Errorf("sentence %d TotalSentences = %d, want 2", i, tdb.TotalSentences)
		}
		if tdb.SentenceNumber != int64(i+1) {
			t.Errorf("sentence %d SentenceNumber = %d, want %d", i, tdb.SentenceNumber, i+1)
		}
		if tdb.DestID != 992359599 {
			t.Errorf("sentence %d DestID = %d, want 992359599", i, tdb.DestID)
		}
		if tdb.Priority != 2 {
			t.Errorf("sentence %d Priority = %d, want 2", i, tdb.Priority)
		}
		chunk, decodeErr := Decode6Bit(tdb.Data)
		if decodeErr != nil {
			t.Fatalf("sentence %d payload not decodable: %v", i, decodeErr)
		}
		rebuilt = append(rebuilt, chunk...)
	}
	if string(rebuilt) != string(payload) {
		t.Fatalf("reassembled payload differs: got %d bytes, want %d", len(rebuilt), len(payload))
	}
}

func TestModemSendShort(t *testing.T) {
	fake := newFakeModem(t)
	m := NewModem(fake.addr(), ModemHandler{})
	m.Start()
	t.Cleanup(m.Stop)
	waitFor(t, "modem link up", m.Healthy)

	if err := m.SendShort(992359599, []byte("hello VDES"), 1, TSBAcknowledged); err != nil {
		t.Fatalf("SendShort failed: %v", err)
	}
	waitFor(t, "one TSB sentence", func() bool { return len(fake.sentences()) == 1 })

	s, err := Parse(fake.sentences()[0])
	if err != nil {
		t.Fatalf("TSB unparsable: %v", err)
	}
	tsb, err := ParseTSB(s)
	if err != nil {
		t.Fatalf("not a TSB: %v", err)
	}
	if tsb.MessageID != TSBAcknowledged {
		t.Errorf("MessageID = %d, want %d", tsb.MessageID, TSBAcknowledged)
	}
	payload, err := Decode6Bit(tsb.Data)
	if err != nil {
		t.Fatalf("payload not decodable: %v", err)
	}
	if string(payload) != "hello VDES" {
		t.Errorf("payload = %q, want %q", payload, "hello VDES")
	}
}

func TestModemSendShortRejectsOversizeAndAckedBroadcast(t *testing.T) {
	fake := newFakeModem(t)
	m := NewModem(fake.addr(), ModemHandler{})
	m.Start()
	t.Cleanup(m.Stop)
	waitFor(t, "modem link up", m.Healthy)

	if err := m.SendShort(0, make([]byte, MaxShortMessageBytes+1), 0, TSBUnacknowledged); err == nil {
		t.Error("SendShort accepted an oversized payload")
	}
	if err := m.SendShort(0, []byte("hi"), 0, TSBAcknowledged); err == nil {
		t.Error("SendShort accepted an acknowledged broadcast")
	}
}

func TestModemReceivesEDMWithESI(t *testing.T) {
	fake := newFakeModem(t)

	type received struct {
		mmsi    int64
		payload []byte
	}
	var mu sync.Mutex
	var got []received

	m := NewModem(fake.addr(), ModemHandler{
		OnMessage: func(mmsi int64, payload []byte) {
			mu.Lock()
			got = append(got, received{mmsi: mmsi, payload: payload})
			mu.Unlock()
		},
	})
	m.Start()
	t.Cleanup(m.Stop)
	waitFor(t, "modem link up", m.Healthy)

	// The peer MMSI arrives in the ESI sentence that follows the EDM.
	fake.push(t, Format(SentenceStart, TalkerVDES, "EDM", []string{"0", "", "992359599", Encode6Bit([]byte("route")), "0"}))
	fake.push(t, Format(SentenceStart, TalkerVDES, "ESI", []string{"0", "992359598", "7"}))

	waitFor(t, "one received message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if string(got[0].payload) != "route" {
		t.Errorf("payload = %q, want route", got[0].payload)
	}
	// The peer MMSI comes from the ESI sentence that follows the EDM.
	if got[0].mmsi != 992359598 {
		t.Errorf("mmsi = %d, want 992359598 (from ESI)", got[0].mmsi)
	}
}

func TestModemReassemblesChainedTDB(t *testing.T) {
	fake := newFakeModem(t)

	var mu sync.Mutex
	var got [][]byte
	m := NewModem(fake.addr(), ModemHandler{
		OnMessage: func(_ int64, payload []byte) {
			mu.Lock()
			got = append(got, payload)
			mu.Unlock()
		},
	})
	m.Start()
	t.Cleanup(m.Stop)
	waitFor(t, "modem link up", m.Healthy)

	// Three chained fragments delivered as separate TDB sentences.
	parts := [][]byte{[]byte("first-"), []byte("middle-"), []byte("last")}
	msgNumbers := []byte{MsgStart, MsgContinue, MsgEnd}
	for i, part := range parts {
		fake.push(t, Format(SentenceStart, TalkerVDES, "TDB", []string{
			"3", // total sentences
			"1", // sentence number
			"9", // message sequence number (conversation key)
			"",  // source
			"992359599",
			"0",
			formatOptionalID(int64(msgNumbers[i])),
			Encode6Bit(part),
			"0",
		}))
	}

	waitFor(t, "reassembled message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if string(got[0]) != "first-middle-last" {
		t.Errorf("reassembled = %q, want first-middle-last", got[0])
	}
}

func TestModemReportsTransmitCompletion(t *testing.T) {
	fake := newFakeModem(t)

	done := make(chan int64, 1)
	m := NewModem(fake.addr(), ModemHandler{
		OnTransmitted: func(destMMSI int64) { done <- destMMSI },
	})
	m.Start()
	t.Cleanup(m.Stop)
	waitFor(t, "modem link up", m.Healthy)

	fake.push(t, Format(SentenceStartEnc, TalkerVendor, "EDO", []string{"0", "992359598", "992359599", Encode6Bit([]byte("x")), "0"}))

	select {
	case dest := <-done:
		if dest != 992359599 {
			t.Errorf("transmit completion destMMSI = %d, want 992359599", dest)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transmit completion was never reported")
	}
}

func TestModemIgnoresGarbage(t *testing.T) {
	fake := newFakeModem(t)

	var mu sync.Mutex
	count := 0
	m := NewModem(fake.addr(), ModemHandler{
		OnMessage: func(int64, []byte) {
			mu.Lock()
			count++
			mu.Unlock()
		},
	})
	m.Start()
	t.Cleanup(m.Stop)
	waitFor(t, "modem link up", m.Healthy)

	fake.push(t, "not a sentence\r\n")
	fake.push(t, "$VDEDM,0,,992359599,BAD,0*00\r\n") // wrong checksum
	fake.push(t, "\r\n")

	// A valid message must still get through after the garbage. Per the standard
	// the EDM is followed by the ESI carrying the peer MMSI.
	fake.push(t, Format(SentenceStart, TalkerVDES, "EDM", []string{"0", "", "992359599", Encode6Bit([]byte("ok")), "0"}))
	fake.push(t, Format(SentenceStart, TalkerVDES, "ESI", []string{"0", "992359598", "7"}))
	waitFor(t, "the valid message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return count == 1
	})
}
