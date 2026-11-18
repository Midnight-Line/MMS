package link

import (
	"testing"

	"github.com/maritimeconnectivity/MMS/mmtp"
	"google.golang.org/protobuf/proto"
)

// fakeLink is a trivial in-memory Link implementation for tests.
type fakeLink struct {
	name     string
	healthy  bool
	sent     []*mmtp.MmtpMessage
	dropNext bool
}

func (f *fakeLink) Name() string  { return f.name }
func (f *fakeLink) Healthy() bool { return f.healthy }
func (f *fakeLink) Send(mm *mmtp.MmtpMessage) error {
	if f.dropNext {
		f.dropNext = false
		return errNoHealthyLink
	}
	f.sent = append(f.sent, mm)
	return nil
}

func mmtpSend(subject string, size uint32) *mmtp.MmtpMessage {
	return &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SEND_MESSAGE,
				Body: &mmtp.ProtocolMessage_SendMessage{
					SendMessage: &mmtp.Send{
						ApplicationMessage: &mmtp.ApplicationMessage{
							Header: &mmtp.ApplicationMessageHeader{
								SubjectOrRecipient: &mmtp.ApplicationMessageHeader_Subject{
									Subject: subject,
								},
								BodySizeNumBytes: size,
							},
							Body: make([]byte, size),
						},
					},
				},
			},
		},
	}
}

func TestParsePreferred(t *testing.T) {
	cases := map[string]Preferred{
		"ip":   PreferredIP,
		"IP":   PreferredIP,
		"vde":  PreferredVDE,
		"VDE":  PreferredVDE,
		"auto": PreferredAuto,
		"":     PreferredAuto,
		"x":    PreferredAuto,
	}
	for in, want := range cases {
		if got := ParsePreferred(in); got != want {
			t.Errorf("ParsePreferred(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPreferredIPRouting(t *testing.T) {
	m := NewManager(Config{Preferred: PreferredIP})
	ip := &fakeLink{name: "ip", healthy: true}
	vde := &fakeLink{name: "vde", healthy: true}
	m.Register(ip)
	m.Register(vde)

	if err := m.Send(mmtpSend("subj", 100)); err != nil {
		t.Fatal(err)
	}
	if len(ip.sent) != 1 || len(vde.sent) != 0 {
		t.Fatalf("expected only IP, got ip=%d vde=%d", len(ip.sent), len(vde.sent))
	}
}

func TestPreferredVDERouting(t *testing.T) {
	m := NewManager(Config{Preferred: PreferredVDE})
	ip := &fakeLink{name: "ip", healthy: true}
	vde := &fakeLink{name: "vde", healthy: true}
	m.Register(ip)
	m.Register(vde)

	if err := m.Send(mmtpSend("subj", 100)); err != nil {
		t.Fatal(err)
	}
	if len(vde.sent) != 1 || len(ip.sent) != 0 {
		t.Fatalf("expected only VDE, got ip=%d vde=%d", len(ip.sent), len(vde.sent))
	}
}

func TestAutoLargePayloadGoesToIP(t *testing.T) {
	m := NewManager(Config{Preferred: PreferredAuto})
	ip := &fakeLink{name: "ip", healthy: true}
	vde := &fakeLink{name: "vde", healthy: true}
	m.Register(ip)
	m.Register(vde)

	if err := m.Send(mmtpSend("subj", 9000)); err != nil {
		t.Fatal(err)
	}
	if len(ip.sent) != 1 {
		t.Fatalf("large payload should go to IP, got ip=%d vde=%d", len(ip.sent), len(vde.sent))
	}
	// Note: auto may also dual-send to vde. Accept that.
}

func TestAutoSmallPayloadPrefersVDE(t *testing.T) {
	m := NewManager(Config{Preferred: PreferredAuto})
	ip := &fakeLink{name: "ip", healthy: true}
	vde := &fakeLink{name: "vde", healthy: true}
	m.Register(ip)
	m.Register(vde)

	if err := m.Send(mmtpSend("subj", 100)); err != nil {
		t.Fatal(err)
	}
	if len(vde.sent) < 1 {
		t.Fatalf("small payload should at least hit VDE, ip=%d vde=%d", len(ip.sent), len(vde.sent))
	}
}

func TestNoHealthyLink(t *testing.T) {
	m := NewManager(Config{Preferred: PreferredIP})
	m.Register(&fakeLink{name: "ip", healthy: false})
	m.Register(&fakeLink{name: "vde", healthy: false})
	err := m.Send(mmtpSend("s", 1))
	if !IsNoHealthyLink(err) {
		t.Fatalf("expected NoHealthyLinkError, got %v", err)
	}
}

func TestSubjectOverride(t *testing.T) {
	m := NewManager(Config{Preferred: PreferredIP})
	ip := &fakeLink{name: "ip", healthy: true}
	vde := &fakeLink{name: "vde", healthy: true}
	m.Register(ip)
	m.Register(vde)

	m.SetSubjectPolicy("alerts", PreferredVDE)
	if err := m.Send(mmtpSend("alerts", 100)); err != nil {
		t.Fatal(err)
	}
	if len(vde.sent) != 1 || len(ip.sent) != 0 {
		t.Fatalf("subject override failed: ip=%d vde=%d", len(ip.sent), len(vde.sent))
	}
}

// ensure proto is linked in (compile-time dependency)
var _ = proto.Size
