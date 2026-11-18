package pi

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	data := []byte("hello VDES world! 0123456789 ABCDEFGHIJKLMNOPQRSTUVWXYZ @[\\]^_`{|}~")
	enc := Encode6Bit(data)
	dec, err := Decode6Bit(enc)
	if err != nil {
		t.Fatalf("Decode6Bit error: %v", err)
	}
	if !bytes.Equal(dec, data) {
		t.Fatalf("round trip mismatch:\n got %q\nwant %q", dec, data)
	}
}

func TestDecode6BitKnownValue(t *testing.T) {
	// 'A' = 1 (000001), '@' = 0, ' ' = 32 (100000).
	// Bit stream (MSB-first): 000001 000000 100000 -> bytes: 00000100 00001000 (0x04, 0x08), trailing 0000 drop.
	dec, err := Decode6Bit("A@ ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, []byte{0x04, 0x08}) {
		t.Fatalf("unexpected decode: % x", dec)
	}
}

func TestAssemblerSingle(t *testing.T) {
	a := NewAssembler()
	full, done := a.Push("mmsi-1", Fragment{MsgNumber: MsgEnd, Data: []byte("abc")})
	if done {
		t.Fatal("single End fragment without Start must not complete")
	}
	if full != nil {
		t.Fatalf("expected nil, got %q", full)
	}

	full, done = a.Push("mmsi-1", Fragment{MsgNumber: MsgStart, Data: []byte("ab")})
	if done {
		t.Fatal("Start alone must not complete")
	}
	full, done = a.Push("mmsi-1", Fragment{MsgNumber: MsgEnd, Data: []byte("cd")})
	if !done {
		t.Fatal("Start+End must complete")
	}
	if !bytes.Equal(full, []byte("abcd")) {
		t.Fatalf("expected abcd, got %q", full)
	}
}

func TestAssemblerMultiFragment(t *testing.T) {
	a := NewAssembler()
	a.Push("s", Fragment{MsgNumber: MsgStart, Data: []byte("01")})
	a.Push("s", Fragment{MsgNumber: MsgContinue, Data: []byte("23")})
	a.Push("s", Fragment{MsgNumber: MsgContinue, Data: []byte("45")})
	full, done := a.Push("s", Fragment{MsgNumber: MsgEnd, Data: []byte("67")})
	if !done {
		t.Fatal("expected completion on End")
	}
	if !bytes.Equal(full, []byte("01234567")) {
		t.Fatalf("reassembly wrong: %q", full)
	}
}

func TestAssemblerInterleavedConversations(t *testing.T) {
	a := NewAssembler()
	a.Push("conv-a", Fragment{MsgNumber: MsgStart, Data: []byte("aa")})
	a.Push("conv-b", Fragment{MsgNumber: MsgStart, Data: []byte("bb")})

	fullA, doneA := a.Push("conv-a", Fragment{MsgNumber: MsgEnd, Data: []byte("aa2")})
	fullB, doneB := a.Push("conv-b", Fragment{MsgNumber: MsgEnd, Data: []byte("bb2")})
	if !doneA || string(fullA) != "aaaa2" {
		t.Fatalf("conv-a: got %q done=%v", fullA, doneA)
	}
	if !doneB || string(fullB) != "bbbb2" {
		t.Fatalf("conv-b: got %q done=%v", fullB, doneB)
	}

	// Both conversations are finished; another End for conv-a must not
	// complete anything (no Start pending).
	full, done := a.Push("conv-a", Fragment{MsgNumber: MsgEnd, Data: []byte("")})
	if done || full != nil {
		t.Fatalf("stale End must be dropped, got %q done=%v", full, done)
	}
}

func TestAssemblerResetOnRestart(t *testing.T) {
	a := NewAssembler()
	a.Push("s", Fragment{MsgNumber: MsgStart, Data: []byte("old")})
	// A new Start resets the in-flight conversation (sender retransmits).
	a.Push("s", Fragment{MsgNumber: MsgStart, Data: []byte("new")})
	full, done := a.Push("s", Fragment{MsgNumber: MsgEnd, Data: []byte("2")})
	if !done || string(full) != "new2" {
		t.Fatalf("expected new2, got %q done=%v", full, done)
	}
}

func TestSplitFragments(t *testing.T) {
	frags, err := SplitFragments([]byte("0123456789"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 3 {
		t.Fatalf("expected 3 fragments, got %d", len(frags))
	}
	if frags[0].MsgNumber != MsgStart || frags[2].MsgNumber != MsgEnd {
		t.Fatal("fragment boundary markers incorrect")
	}
	var joined []byte
	for _, f := range frags {
		joined = append(joined, f.Data...)
	}
	if !bytes.Equal(joined, []byte("0123456789")) {
		t.Fatalf("joined = %q", joined)
	}
}

func TestSplitFragmentsSingle(t *testing.T) {
	frags, _ := SplitFragments([]byte("hello"), 100)
	if len(frags) != 1 || frags[0].MsgNumber != MsgStart {
		t.Fatalf("expected single start fragment, got %+v", frags)
	}
	if !bytes.Equal(frags[0].Data, []byte("hello")) {
		t.Fatalf("data mismatch %q", frags[0].Data)
	}
}
