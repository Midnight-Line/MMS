package pi

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Sentence framing constants, following the NMEA 0183 / IEC 61162 style used by
// VDE ship modems: a sentence starts with '$' (or '!' for encapsulated data),
// carries comma separated fields, and ends with '*hh' plus CR LF where hh is the
// XOR checksum of everything between the start character and the '*'.
const (
	SentenceStart    byte = '$'
	SentenceStartEnc byte = '!'
	SentenceEnd           = "\r\n"
	ChecksumSep      byte = '*'
)

// Talker identifiers. The VDES talker for IEC sentences is "VD"; some modem
// vendors use "VE" for proprietary sentences (for example VEEDO/VEEAK).
const (
	TalkerVDES   = "VD"
	TalkerVendor = "VE"
)

// Sentence is a parsed NMEA style sentence.
type Sentence struct {
	Start     byte     // '$' or '!'
	Talker    string   // two character talker ID, e.g. "VD"
	Formatter string   // three character sentence formatter, e.g. "TDB"
	Fields    []string // comma separated data fields
}

// ErrChecksum is returned when a sentence carries an invalid checksum.
var ErrChecksum = errors.New("pi: invalid sentence checksum")

// ErrMalformed is returned when a sentence cannot be split into its parts.
var ErrMalformed = errors.New("pi: malformed sentence")

// Checksum computes the XOR checksum over the payload (the characters between
// the start character and the '*').
func Checksum(payload string) byte {
	var sum byte
	for i := 0; i < len(payload); i++ {
		sum ^= payload[i]
	}
	return sum
}

// Format renders a sentence with the given start character, talker, formatter
// and fields, appending the checksum and CR LF.
func Format(start byte, talker, formatter string, fields []string) string {
	payload := talker + formatter
	if len(fields) > 0 {
		payload += "," + strings.Join(fields, ",")
	}
	return fmt.Sprintf("%c%s*%02X%s", start, payload, Checksum(payload), SentenceEnd)
}

// Parse splits a single sentence into its parts and validates the checksum.
func Parse(line string) (Sentence, error) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 2 {
		return Sentence{}, ErrMalformed
	}
	start := line[0]
	if start != SentenceStart && start != SentenceStartEnc {
		return Sentence{}, fmt.Errorf("%w: does not start with '$' or '!'", ErrMalformed)
	}

	star := strings.LastIndexByte(line, ChecksumSep)
	if star < 0 || star+3 > len(line) {
		return Sentence{}, fmt.Errorf("%w: missing checksum", ErrMalformed)
	}
	payload := line[1:star]
	want, err := strconv.ParseUint(line[star+1:], 16, 8)
	if err != nil {
		return Sentence{}, fmt.Errorf("%w: unreadable checksum %q", ErrMalformed, line[star+1:])
	}
	if got := Checksum(payload); got != byte(want) {
		return Sentence{}, fmt.Errorf("%w: got %02X want %02X", ErrChecksum, got, want)
	}

	parts := strings.Split(payload, ",")
	head := parts[0]
	if len(head) < 5 {
		return Sentence{}, fmt.Errorf("%w: header %q too short", ErrMalformed, head)
	}

	s := Sentence{
		Start:     start,
		Talker:    head[:2],
		Formatter: head[2:],
		Fields:    nil,
	}
	if len(parts) > 1 {
		s.Fields = parts[1:]
	}
	return s, nil
}

// field returns the i-th data field or an empty string when absent.
func (s Sentence) field(i int) string {
	if i < 0 || i >= len(s.Fields) {
		return ""
	}
	return s.Fields[i]
}

// splitData separates a sentence whose payload field may itself contain commas.
// The 6-bit ASCII alphabet includes ',' and '*', so a naive field split would
// shift every field after the payload. Given the index of the payload field,
// everything between it and the final field (the fill bits) is rejoined.
func (s Sentence) splitData(dataIndex int) (head []string, data string, tail string, err error) {
	if len(s.Fields) < dataIndex+2 {
		return nil, "", "", fmt.Errorf("%w: expected at least %d fields, got %d", ErrMalformed, dataIndex+2, len(s.Fields))
	}
	head = s.Fields[:dataIndex]
	tail = s.Fields[len(s.Fields)-1]
	data = strings.Join(s.Fields[dataIndex:len(s.Fields)-1], ",")
	return head, data, tail, nil
}

// intField parses the i-th data field as an integer, treating an empty field as 0.
func (s Sentence) intField(i int) (int64, error) {
	return parseIntField(s.field(i), i)
}

// parseIntField parses one field as an integer, treating an empty field as 0.
func parseIntField(raw string, index int) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: field %d (%q) is not a number", ErrMalformed, index+1, raw)
	}
	return v, nil
}

// TDB is a VDE long message sentence (transmit and receive). It carries one
// fragment of a long message; fragments are chained through the internal message
// number (74 start, 75 continue, 76 end, 73 acknowledgement).
//
// Field order: total sentences, sentence number, message sequence number,
// source ID, destination ID, priority, data type, data, fill bits.
type TDB struct {
	TotalSentences int64  // 1..99, total sentences belonging to this message
	SentenceNumber int64  // 1..99, position of this sentence
	MessageSeq     int64  // 0..99, sequence number of the message
	SourceID       int64  // 0..4294967295, left empty when transmitting
	DestID         int64  // 0..4294967295, left empty for broadcasts
	Priority       int64  // 0..2, 2 is highest
	DataType       int64  // 0..255, reserved, currently 0
	Data           string // 6-bit ASCII encoded payload
	FillBits       int64  // 0..5 padding bits in the final 6-bit character
}

// Sentence renders the TDB sentence.
func (t TDB) Sentence(talker string) string {
	return Format(SentenceStart, talker, "TDB", []string{
		strconv.FormatInt(t.TotalSentences, 10),
		strconv.FormatInt(t.SentenceNumber, 10),
		strconv.FormatInt(t.MessageSeq, 10),
		formatOptionalID(t.SourceID),
		formatOptionalID(t.DestID),
		strconv.FormatInt(t.Priority, 10),
		strconv.FormatInt(t.DataType, 10),
		t.Data,
		strconv.FormatInt(t.FillBits, 10),
	})
}

// ParseTDB interprets a parsed sentence as a TDB sentence.
func ParseTDB(s Sentence) (TDB, error) {
	if s.Formatter != "TDB" {
		return TDB{}, fmt.Errorf("%w: formatter %q is not TDB", ErrMalformed, s.Formatter)
	}
	head, data, tail, err := s.splitData(7)
	if err != nil {
		return TDB{}, err
	}

	var t TDB
	if t.TotalSentences, err = parseIntField(head[0], 0); err != nil {
		return TDB{}, err
	}
	if t.SentenceNumber, err = parseIntField(head[1], 1); err != nil {
		return TDB{}, err
	}
	if t.MessageSeq, err = parseIntField(head[2], 2); err != nil {
		return TDB{}, err
	}
	if t.SourceID, err = parseIntField(head[3], 3); err != nil {
		return TDB{}, err
	}
	if t.DestID, err = parseIntField(head[4], 4); err != nil {
		return TDB{}, err
	}
	if t.Priority, err = parseIntField(head[5], 5); err != nil {
		return TDB{}, err
	}
	if t.DataType, err = parseIntField(head[6], 6); err != nil {
		return TDB{}, err
	}
	t.Data = data
	if t.FillBits, err = parseIntField(tail, 8); err != nil {
		return TDB{}, err
	}
	return t, nil
}

// TSB is a VDE short message sentence. Short messages always fit in a single
// sentence. Message ID 92 requests an acknowledgement, 93 does not (and must be
// used for broadcasts).
//
// Field order: total sentences, sentence number, message sequence number,
// source ID, destination ID, priority, message ID, data, fill bits.
type TSB struct {
	TotalSentences int64
	SentenceNumber int64
	MessageSeq     int64
	SourceID       int64
	DestID         int64
	Priority       int64
	MessageID      int64  // 92 (acknowledged) or 93 (unacknowledged / broadcast)
	Data           string // 6-bit ASCII encoded payload
	FillBits       int64
}

// Short message identifiers.
const (
	TSBAcknowledged   int64 = 92
	TSBUnacknowledged int64 = 93
)

// Sentence renders the TSB sentence.
func (t TSB) Sentence(talker string) string {
	return Format(SentenceStart, talker, "TSB", []string{
		strconv.FormatInt(t.TotalSentences, 10),
		strconv.FormatInt(t.SentenceNumber, 10),
		strconv.FormatInt(t.MessageSeq, 10),
		formatOptionalID(t.SourceID),
		formatOptionalID(t.DestID),
		strconv.FormatInt(t.Priority, 10),
		strconv.FormatInt(t.MessageID, 10),
		t.Data,
		strconv.FormatInt(t.FillBits, 10),
	})
}

// ParseTSB interprets a parsed sentence as a TSB sentence.
func ParseTSB(s Sentence) (TSB, error) {
	if s.Formatter != "TSB" {
		return TSB{}, fmt.Errorf("%w: formatter %q is not TSB", ErrMalformed, s.Formatter)
	}
	head, data, tail, err := s.splitData(7)
	if err != nil {
		return TSB{}, err
	}

	var t TSB
	if t.TotalSentences, err = parseIntField(head[0], 0); err != nil {
		return TSB{}, err
	}
	if t.SentenceNumber, err = parseIntField(head[1], 1); err != nil {
		return TSB{}, err
	}
	if t.MessageSeq, err = parseIntField(head[2], 2); err != nil {
		return TSB{}, err
	}
	if t.SourceID, err = parseIntField(head[3], 3); err != nil {
		return TSB{}, err
	}
	if t.DestID, err = parseIntField(head[4], 4); err != nil {
		return TSB{}, err
	}
	if t.Priority, err = parseIntField(head[5], 5); err != nil {
		return TSB{}, err
	}
	if t.MessageID, err = parseIntField(head[6], 6); err != nil {
		return TSB{}, err
	}
	t.Data = data
	if t.FillBits, err = parseIntField(tail, 8); err != nil {
		return TSB{}, err
	}
	return t, nil
}

// EDM reports a message that the modem received over the air and is handing to
// the Edge Router. The peer MMSI is carried in the accompanying ESI sentence.
//
// Field order: sequence number, source ID, destination ID, data, fill bits.
type EDM struct {
	Seq      int64
	SourceID int64
	DestID   int64
	Data     string
	FillBits int64
}

// ParseEDM interprets a parsed sentence as an EDM sentence.
func ParseEDM(s Sentence) (EDM, error) {
	if s.Formatter != "EDM" {
		return EDM{}, fmt.Errorf("%w: formatter %q is not EDM", ErrMalformed, s.Formatter)
	}
	head, data, tail, err := s.splitData(3)
	if err != nil {
		return EDM{}, err
	}

	var e EDM
	if e.Seq, err = parseIntField(head[0], 0); err != nil {
		return EDM{}, err
	}
	if e.SourceID, err = parseIntField(head[1], 1); err != nil {
		return EDM{}, err
	}
	if e.DestID, err = parseIntField(head[2], 2); err != nil {
		return EDM{}, err
	}
	e.Data = data
	if e.FillBits, err = parseIntField(tail, 4); err != nil {
		return EDM{}, err
	}
	return e, nil
}

// ESI reports time slot / signal information and always follows an EDM or EDO
// sentence. The peer MMSI of the transfer is carried here.
//
// Field order: sequence number, MMSI, then modem specific slot information.
type ESI struct {
	Seq  int64
	MMSI int64
	Rest []string
}

// ParseESI interprets a parsed sentence as an ESI sentence.
func ParseESI(s Sentence) (ESI, error) {
	if s.Formatter != "ESI" {
		return ESI{}, fmt.Errorf("%w: formatter %q is not ESI", ErrMalformed, s.Formatter)
	}
	var e ESI
	var err error
	if e.Seq, err = s.intField(0); err != nil {
		return ESI{}, err
	}
	if e.MMSI, err = s.intField(1); err != nil {
		return ESI{}, err
	}
	if len(s.Fields) > 2 {
		e.Rest = append(e.Rest, s.Fields[2:]...)
	}
	return e, nil
}

// EDO reports that the modem has transmitted a message.
//
// Field order: sequence number, source ID, destination ID, data, fill bits.
type EDO struct {
	Seq      int64
	SourceID int64
	DestID   int64
	Data     string
	FillBits int64
}

// ParseEDO interprets a parsed sentence as an EDO sentence.
func ParseEDO(s Sentence) (EDO, error) {
	if s.Formatter != "EDO" {
		return EDO{}, fmt.Errorf("%w: formatter %q is not EDO", ErrMalformed, s.Formatter)
	}
	head, data, tail, err := s.splitData(3)
	if err != nil {
		return EDO{}, err
	}

	var e EDO
	if e.Seq, err = parseIntField(head[0], 0); err != nil {
		return EDO{}, err
	}
	if e.SourceID, err = parseIntField(head[1], 1); err != nil {
		return EDO{}, err
	}
	if e.DestID, err = parseIntField(head[2], 2); err != nil {
		return EDO{}, err
	}
	e.Data = data
	if e.FillBits, err = parseIntField(tail, 4); err != nil {
		return EDO{}, err
	}
	return e, nil
}

// formatOptionalID renders an ID field, leaving it empty when the ID is unset.
// The standard requires source IDs to be omitted when transmitting and
// destination IDs to be omitted for broadcasts.
func formatOptionalID(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
