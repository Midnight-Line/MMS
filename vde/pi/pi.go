// Package pi implements the ship-side PI (Presentation Interface) layer that
// translates between the Edge Router and the VDES ship-board modem.
//
// The modem connection is a lightweight Ethernet/NMEA link. The ER sends
// downlink payloads as TDB (long message) / TSB (short message) statement
// sends; the modem acknowledges with TMK / TSK; uplink messages arrive from
// the modem as EDM statements, whose peer MMSI is carried in an associated
// ESI statement; a sent downlink is confirmed with EDO.
//
// Payload data on a VDES PI link is 6-bit-encoded ASCII. Long payloads are
// split into fragments distinguished by the internal message number
// (74 = start, 75 = continuation, 76 = end, 73 = acknowledgement).
//
// NOTE ON THE STATEMENT GRAMMAR: the authoritative IEC 63514 draft (A.3.1,
// TDB PI statement field numbering) is not in the workspace, and vendor
// statement layouts differ. This package therefore pins down the parts that
// are well-defined -- the 6-bit character set (ITU-R M.1371) and the
// fragmentation/reassembly state machine -- and keeps the statement framing
// deliberately data-oriented so a concrete vendor grammar can be bound in the
// modem driver layer without disturbing reassembly.
package pi

import (
	"errors"
	"fmt"
	"sync"
)

// Fragment message numbers, per the VDES PI / base-station sentence design
// (docs/基站VDES语句设计0821.docx).
const (
	MsgStart      byte = 74 // first fragment of a long message
	MsgContinue   byte = 75 // middle fragment
	MsgEnd        byte = 76 // final fragment of a long message
	MsgAck        byte = 73
	MsgResRequest byte = 90
	MsgResGrant   byte = 4
)

// --- 6-bit ASCII (ITU-R M.1371-5) --------------------------------

var dec6bit = [128]byte{}
var enc6bit = [64]byte{}

func init() {
	// 000000 = '@'
	enc6bit[0] = '@'
	// 1..26 = 'A'..'Z'
	for i := 1; i <= 26; i++ {
		enc6bit[i] = byte('A' + i - 1)
	}
	// 27..31 = '[','\',']','^','_'
	enc6bit[27] = '['
	enc6bit[28] = '\\'
	enc6bit[29] = ']'
	enc6bit[30] = '^'
	enc6bit[31] = '_'
	// 32 = space
	enc6bit[32] = ' '
	// 33..47 = '!'..'/'
	for i := 0; i < 15; i++ {
		enc6bit[33+i] = byte('!' + i)
	}
	// 48..57 = '0'..'9'
	for i := 0; i < 10; i++ {
		enc6bit[48+i] = byte('0' + i)
	}
	// 58..63 = ':'..'?'
	enc6bit[58] = ':'
	enc6bit[59] = ';'
	enc6bit[60] = '<'
	enc6bit[61] = '='
	enc6bit[62] = '>'
	enc6bit[63] = '?'

	for i := 0; i < len(enc6bit); i++ {
		dec6bit[enc6bit[i]] = byte(i)
	}
}

// Encode6Bit packs an arbitrary byte slice into the VDES 6-bit ASCII
// character set using the AIS/M.1371 bit-packing convention: the input is
// treated as one MSB-first bit stream, grouped 6 bits at a time, and each
// 6-bit group is emitted as its character. A final partial group is zero
// padded. This round-trips arbitrary binary payloads (e.g. a marshalled
// MMTP message) losslessly.
func Encode6Bit(data []byte) string {
	var out []byte
	var acc uint32
	var nbits uint
	for _, b := range data {
		acc = (acc << 8) | uint32(b)
		nbits += 8
		for nbits >= 6 {
			nbits -= 6
			v := (acc >> nbits) & 0x3F
			out = append(out, enc6bit[v])
		}
	}
	if nbits > 0 {
		v := (acc << (6 - nbits)) & 0x3F
		out = append(out, enc6bit[v])
	}
	return string(out)
}

// Decode6Bit unpacks a VDES 6-bit ASCII string back into its original bytes.
// Trailing padding bits in the final partial group are dropped.
func Decode6Bit(s string) ([]byte, error) {
	var out []byte
	var acc uint32
	var nbits uint
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 128 {
			return nil, fmt.Errorf("pi: non-ASCII char 0x%x at %d", c, i)
		}
		v := dec6bit[c]
		acc = (acc << 6) | uint32(v)
		nbits += 6
		for nbits >= 8 {
			nbits -= 8
			out = append(out, byte((acc>>nbits)&0xFF))
		}
	}
	return out, nil
}

// --- Fragment reassembly -------------------------------------------

// Fragment is one PI-layer fragment of a fragmented long message.
type Fragment struct {
	MsgNumber byte // MsgStart / MsgContinue / MsgEnd
	// Sequence/offset bookkeeping is the sender's; the assembler keys purely
	// on MsgNumber plus a conversation key supplied by the caller.
	Data []byte // 6-bit-decoded fragment payload
}

// Assembler reassembles inbound fragments (MsgStart -> MsgContinue* ->
// MsgEnd) into a complete payload. It is safe for concurrent use by a single
// uplink goroutine.
type Assembler struct {
	mu      sync.Mutex
	pending map[string]*partial
}

type partial struct {
	msgNum byte // last write: Start or Continue
	data   []byte
}

// NewAssembler returns an empty reassembler.
func NewAssembler() *Assembler {
	return &Assembler{pending: make(map[string]*partial)}
}

// Push ingests one inbound fragment keyed by conv (e.g. source MMSI for a
// dedicated conversation, or a per-slot identifier). Returns the complete
// payload and true once the terminating fragment (MsgEnd) arrives; returns
// (nil,false) while more fragments are expected. A fragment that violates the
// state machine (e.g. a second MsgStart for an in-flight conversation) resets
// the conversation.
func (a *Assembler) Push(conv string, f Fragment) ([]byte, bool) {
	if len(conv) == 0 {
		conv = "_default_"
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	switch f.MsgNumber {
	case MsgStart:
		// Begin fresh; discard any in-flight partial.
		a.pending[conv] = &partial{msgNum: MsgStart, data: f.Data}
		return nil, false
	case MsgContinue, MsgEnd:
		p, ok := a.pending[conv]
		if !ok {
			// Continuation with no start: invalid, drop.
			return nil, false
		}
		p.data = append(p.data, f.Data...)
		if f.MsgNumber == MsgEnd {
			full := p.data
			delete(a.pending, conv)
			return full, true
		}
		p.msgNum = MsgContinue
		return nil, false
	default:
		// ACK / resource requests are not payload fragments.
		return nil, false
	}
}

// Abort discards any in-progress conversation (e.g. on a connectivity drop).
func (a *Assembler) Abort(conv string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if conv == "" {
		conv = "_default_"
	}
	delete(a.pending, conv)
}

// SplitFragments takes a possibly-overlong payload and splits it into PI
// fragments no larger than max per fragment. The split uses MsgStart for the
// first piece and MsgEnd for the last, with MsgContinue in between.
func SplitFragments(data []byte, max int) ([]Fragment, error) {
	if max <= 0 {
		return nil, errors.New("pi: invalid fragment size")
	}
	if len(data) <= max {
		return []Fragment{{MsgNumber: MsgStart, Data: data}}, nil
	}
	var frags []Fragment
	for off := 0; off < len(data); {
		n := len(data) - off
		if n > max {
			n = max
		}
		part := data[off : off+n]
		switch {
		case off == 0:
			frags = append(frags, Fragment{MsgNumber: MsgStart, Data: part})
		case off+n == len(data):
			frags = append(frags, Fragment{MsgNumber: MsgEnd, Data: part})
		default:
			frags = append(frags, Fragment{MsgNumber: MsgContinue, Data: part})
		}
		off += n
	}
	return frags, nil
}
