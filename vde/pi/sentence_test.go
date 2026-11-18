package pi

import (
	"strings"
	"testing"
)

func TestChecksum(t *testing.T) {
	if got := Checksum(""); got != 0 {
		t.Fatalf("Checksum(\"\") = %d, want 0", got)
	}
	// XOR is its own inverse: checksumming the same payload twice cancels out.
	payload := "VDTDB,1,1,0,,992359599,0,0,ABC,0"
	if got := Checksum(payload) ^ Checksum(payload); got != 0 {
		t.Fatalf("XOR of identical checksums = %d, want 0", got)
	}
}

func TestFormatAndParseRoundTrip(t *testing.T) {
	line := Format(SentenceStart, TalkerVDES, "TDB", []string{"1", "1", "0", "", "992359599", "2", "0", "0@ABC", "0"})
	if !strings.HasPrefix(line, "$VDTDB,") {
		t.Fatalf("sentence %q does not start with $VDTDB,", line)
	}
	if !strings.HasSuffix(line, "\r\n") {
		t.Fatalf("sentence %q does not end with CR LF", line)
	}

	s, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse(%q) failed: %v", line, err)
	}
	if s.Start != '$' {
		t.Errorf("Start = %q, want '$'", s.Start)
	}
	if s.Talker != TalkerVDES {
		t.Errorf("Talker = %q, want %q", s.Talker, TalkerVDES)
	}
	if s.Formatter != "TDB" {
		t.Errorf("Formatter = %q, want TDB", s.Formatter)
	}
	if len(s.Fields) != 9 {
		t.Errorf("len(Fields) = %d, want 9", len(s.Fields))
	}
}

func TestParseRejectsBadChecksum(t *testing.T) {
	line := Format(SentenceStart, TalkerVDES, "TDB", []string{"1", "1", "0", "", "", "0", "0", "AB", "0"})
	corrupted := strings.Replace(line, "AB", "AC", 1)
	if _, err := Parse(corrupted); err == nil {
		t.Fatal("Parse accepted a sentence with a corrupted payload")
	} else if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %v, want a checksum error", err)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, line := range []string{"", "V", "$VDTDB", "$VD*ZZ", "$VDTDB,1,1*00\r\n"} {
		if _, err := Parse(line); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", line)
		}
	}
}

func TestTDBRoundTrip(t *testing.T) {
	in := TDB{
		TotalSentences: 3,
		SentenceNumber: 2,
		MessageSeq:     17,
		SourceID:       0, // omitted when transmitting
		DestID:         992359599,
		Priority:       2,
		DataType:       0,
		Data:           "0@ABC123",
		FillBits:       4,
	}
	s, err := Parse(in.Sentence(TalkerVDES))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	out, err := ParseTDB(s)
	if err != nil {
		t.Fatalf("ParseTDB failed: %v", err)
	}
	if in != out {
		t.Fatalf("round trip mismatch:\n in = %+v\nout = %+v", in, out)
	}
}

func TestTDBOmittedIDsBecomeZero(t *testing.T) {
	line := Format(SentenceStart, TalkerVDES, "TDB", []string{"1", "1", "0", "", "", "0", "0", "AB", "0"})
	s, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	tdb, err := ParseTDB(s)
	if err != nil {
		t.Fatalf("ParseTDB failed: %v", err)
	}
	if tdb.SourceID != 0 || tdb.DestID != 0 {
		t.Fatalf("broadcast IDs = (%d, %d), want both zero", tdb.SourceID, tdb.DestID)
	}
}

func TestTSBRoundTrip(t *testing.T) {
	in := TSB{
		TotalSentences: 1,
		SentenceNumber: 1,
		MessageSeq:     3,
		DestID:         992359599,
		Priority:       1,
		MessageID:      TSBUnacknowledged,
		Data:           "0@HELLO",
		FillBits:       2,
	}
	s, err := Parse(in.Sentence(TalkerVDES))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	out, err := ParseTSB(s)
	if err != nil {
		t.Fatalf("ParseTSB failed: %v", err)
	}
	if in != out {
		t.Fatalf("round trip mismatch:\n in = %+v\nout = %+v", in, out)
	}
}

func TestParseEDMAndESI(t *testing.T) {
	// Mirrors the documented CML sample shape: EDM carries the payload, ESI the peer MMSI.
	edmLine := Format(SentenceStart, TalkerVDES, "EDM", []string{"0", "", "992359599", "0000ABC", "2"})
	s, err := Parse(edmLine)
	if err != nil {
		t.Fatalf("Parse(EDM) failed: %v", err)
	}
	edm, err := ParseEDM(s)
	if err != nil {
		t.Fatalf("ParseEDM failed: %v", err)
	}
	if edm.DestID != 992359599 {
		t.Errorf("EDM DestID = %d, want 992359599", edm.DestID)
	}
	if edm.Data != "0000ABC" {
		t.Errorf("EDM Data = %q, want 0000ABC", edm.Data)
	}
	if edm.FillBits != 2 {
		t.Errorf("EDM FillBits = %d, want 2", edm.FillBits)
	}

	esiLine := Format(SentenceStart, TalkerVDES, "ESI", []string{"0", "992359598", "12", "3"})
	s, err = Parse(esiLine)
	if err != nil {
		t.Fatalf("Parse(ESI) failed: %v", err)
	}
	esi, err := ParseESI(s)
	if err != nil {
		t.Fatalf("ParseESI failed: %v", err)
	}
	if esi.MMSI != 992359598 {
		t.Errorf("ESI MMSI = %d, want 992359598", esi.MMSI)
	}
	if len(esi.Rest) != 2 || esi.Rest[0] != "12" || esi.Rest[1] != "3" {
		t.Errorf("ESI Rest = %v, want [12 3]", esi.Rest)
	}
}

func TestParseEDO(t *testing.T) {
	line := Format(SentenceStartEnc, TalkerVendor, "EDO", []string{"0", "992359598", "992359599", "0000ABC", "2"})
	s, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if s.Start != '!' {
		t.Errorf("Start = %q, want '!'", s.Start)
	}
	if s.Talker != TalkerVendor {
		t.Errorf("Talker = %q, want %q", s.Talker, TalkerVendor)
	}

	edo, err := ParseEDO(s)
	if err != nil {
		t.Fatalf("ParseEDO failed: %v", err)
	}
	if edo.SourceID != 992359598 || edo.DestID != 992359599 {
		t.Errorf("EDO IDs = (%d, %d), want (992359598, 992359599)", edo.SourceID, edo.DestID)
	}
}

func TestWrongFormatterRejected(t *testing.T) {
	line := Format(SentenceStart, TalkerVDES, "TSB", []string{"1", "1", "0", "", "", "0", "93", "AB", "0"})
	s, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if _, err := ParseTDB(s); err == nil {
		t.Fatal("ParseTDB accepted a TSB sentence")
	}
}
