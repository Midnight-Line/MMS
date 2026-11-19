// Command testagent is a minimal MMS agent for the deploy test bed.
//
// It connects to an Edge Router over TLS websocket, authenticates with an MCP
// certificate, and can subscribe to subjects or MRNs, send application
// messages and print whatever it receives. The MMS reference implementation
// does not ship such a client, and the Java simulator used by the older test
// scripts is not part of this repository, so the deploy test bed carries its
// own.
//
// Message signing follows utils/auth.VerifySignatureOnMessage exactly: the
// signature covers the subject (or the concatenated recipients), the expiry,
// the sender, the optional QoS profile, the body size and the body.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/maritimeconnectivity/MMS/mmtp"
)

// uidOID is the UID attribute that carries the MRN inside a certificate.
var uidOID = []int{0, 9, 2342, 19200300, 100, 1, 1}

type agent struct {
	conn      *websocket.Conn
	mrn       string
	key       *ecdsa.PrivateKey
	writeMu   sync.Mutex
	pending   map[string]chan *mmtp.ResponseMessage
	pendingMu sync.Mutex
	received  int
	notified  int
}

func main() {
	erAddr := flag.String("er", "wss://127.0.0.1:8888", "Edge Router websocket URL")
	mrn := flag.String("mrn", "", "MRN of this agent, defaults to the certificate UID")
	certPath := flag.String("cert", "", "Client certificate (PEM)")
	keyPath := flag.String("key", "", "Client private key (PEM)")
	caPath := flag.String("ca", "", "CA bundle used to verify the Edge Router (PEM)")
	subscribe := flag.String("subscribe", "", "Comma separated subjects or MRNs to subscribe to")
	direct := flag.Bool("direct", false, "Subscribe to direct messages addressed to this agent's MRN")
	sendTo := flag.String("send-to", "", "Comma separated recipient MRNs for -send")
	sendSubject := flag.String("send-subject", "", "Subject for -send")
	sendBody := flag.String("send", "", "Body to send once the subscriptions are in place")
	qos := flag.String("qos", "", "QoS profile for -send")
	listen := flag.Duration("listen", 15*time.Second, "How long to keep listening after sending")
	expires := flag.Duration("expires", time.Hour, "Expiry of sent messages")
	insecure := flag.Bool("insecure", false, "Skip verification of the Edge Router certificate")
	flag.Parse()

	if *certPath == "" || *keyPath == "" {
		fmt.Fprintln(os.Stderr, "testagent: -cert and -key are required")
		os.Exit(2)
	}

	key, err := loadKey(*keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testagent: %v\n", err)
		os.Exit(1)
	}

	cert, err := loadCert(*certPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testagent: %v\n", err)
		os.Exit(1)
	}

	if *mrn == "" {
		*mrn = mrnFromCert(cert)
	}
	if *mrn == "" {
		fmt.Fprintln(os.Stderr, "testagent: the certificate carries no UID and -mrn was not given")
		os.Exit(2)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
	if *caPath != "" {
		pool, err := loadPool(*caPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testagent: %v\n", err)
			os.Exit(1)
		}
		tlsConfig.RootCAs = pool
	}
	if *insecure {
		tlsConfig.InsecureSkipVerify = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, *erAddr, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "testagent: could not connect to %s: %v\n", *erAddr, err)
		os.Exit(1)
	}
	conn.SetReadLimit(-1)
	defer conn.CloseNow()

	a := &agent{conn: conn, mrn: *mrn, key: key, pending: map[string]chan *mmtp.ResponseMessage{}}

	// The read loop has to run before the first request, otherwise the response
	// to the connect message is never picked up and every request times out.
	go a.readLoop(context.Background())

	if err := a.connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "testagent: connect failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[agent %s] connected to %s\n", a.mrn, *erAddr)

	for _, subject := range splitList(*subscribe) {
		if err := a.subscribe(ctx, subject); err != nil {
			fmt.Fprintf(os.Stderr, "testagent: subscribing to %s failed: %v\n", subject, err)
			os.Exit(1)
		}
		fmt.Printf("[agent %s] subscribed to subject %s\n", a.mrn, subject)
	}

	if *direct {
		if err := a.subscribeDirect(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "testagent: subscribing to direct messages failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[agent %s] subscribed to direct messages\n", a.mrn)
	}

	if *sendBody != "" {
		recipients := splitList(*sendTo)
		if *sendSubject == "" && len(recipients) == 0 {
			fmt.Fprintln(os.Stderr, "testagent: -send needs -send-subject or -send-to")
			os.Exit(2)
		}
		if err := a.send(ctx, recipients, *sendSubject, []byte(*sendBody), *qos, *expires); err != nil {
			fmt.Fprintf(os.Stderr, "testagent: send failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[agent %s] sent %d bytes\n", a.mrn, len(*sendBody))
	}

	time.Sleep(*listen)

	a.disconnect(context.Background())
	fmt.Printf(
		"[agent %s] done, %d notification(s) and %d message(s) received\n",
		a.mrn, a.notified, a.received,
	)
}

// -- protocol ------------------------------------------------------------

func (a *agent) connect(ctx context.Context) error {
	ownMrn := a.mrn
	msg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_CONNECT_MESSAGE,
				Body: &mmtp.ProtocolMessage_ConnectMessage{
					ConnectMessage: &mmtp.Connect{OwnMrn: &ownMrn},
				},
			},
		},
	}

	resp, err := a.request(ctx, msg)
	if err != nil {
		return err
	}
	if resp.GetResponse() != mmtp.ResponseEnum_GOOD {
		return fmt.Errorf("edge router refused the connect: %s", resp.GetReasonText())
	}
	return nil
}

func (a *agent) subscribe(ctx context.Context, subject string) error {
	msg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE,
				Body: &mmtp.ProtocolMessage_SubscribeMessage{
					SubscribeMessage: &mmtp.Subscribe{
						SubjectOrDirectMessages: &mmtp.Subscribe_Subject{Subject: subject},
					},
				},
			},
		},
	}
	resp, err := a.request(ctx, msg)
	if err != nil {
		return err
	}
	if resp.GetResponse() != mmtp.ResponseEnum_GOOD {
		return errors.New(resp.GetReasonText())
	}
	return nil
}

func (a *agent) subscribeDirect(ctx context.Context) error {
	msg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE,
				Body: &mmtp.ProtocolMessage_SubscribeMessage{
					SubscribeMessage: &mmtp.Subscribe{
						SubjectOrDirectMessages: &mmtp.Subscribe_DirectMessages{DirectMessages: true},
					},
				},
			},
		},
	}
	resp, err := a.request(ctx, msg)
	if err != nil {
		return err
	}
	if resp.GetResponse() != mmtp.ResponseEnum_GOOD {
		return errors.New(resp.GetReasonText())
	}
	return nil
}

func (a *agent) send(ctx context.Context, recipients []string, subject string, body []byte, qos string, expires time.Duration) error {
	header := &mmtp.ApplicationMessageHeader{
		Expires:          time.Now().Add(expires).Unix(),
		Sender:           a.mrn,
		BodySizeNumBytes: uint32(len(body)),
	}
	if qos != "" {
		header.QosProfile = &qos
	}
	if len(recipients) > 0 {
		header.SubjectOrRecipient = &mmtp.ApplicationMessageHeader_Recipients{
			Recipients: &mmtp.Recipients{Recipients: recipients},
		}
	} else {
		header.SubjectOrRecipient = &mmtp.ApplicationMessageHeader_Subject{Subject: subject}
	}

	signature, err := a.sign(recipients, subject, header, body)
	if err != nil {
		return err
	}

	appMessage := &mmtp.ApplicationMessage{Header: header, Body: body, Signature: signature}

	msg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_SEND_MESSAGE,
				Body: &mmtp.ProtocolMessage_SendMessage{
					SendMessage: &mmtp.Send{ApplicationMessage: appMessage},
				},
			},
		},
	}

	resp, err := a.request(ctx, msg)
	if err != nil {
		return err
	}
	if resp.GetResponse() != mmtp.ResponseEnum_GOOD {
		return errors.New(resp.GetReasonText())
	}
	return nil
}

// sign reproduces utils/auth.VerifySignatureOnMessage: the byte string that is
// verified is the addressing information, the expiry, the sender, the optional
// QoS profile, the body size and the body, in that order.
func (a *agent) sign(recipients []string, subject string, header *mmtp.ApplicationMessageHeader, body []byte) ([]byte, error) {
	var toBeVerified []byte
	if len(recipients) > 0 {
		for _, recipient := range recipients {
			toBeVerified = append(toBeVerified, []byte(recipient)...)
		}
	} else {
		toBeVerified = append(toBeVerified, []byte(subject)...)
	}
	toBeVerified = append(toBeVerified, []byte(strconv.FormatInt(header.GetExpires(), 10))...)
	toBeVerified = append(toBeVerified, []byte(header.GetSender())...)
	if header.GetQosProfile() != "" {
		toBeVerified = append(toBeVerified, []byte(header.GetQosProfile())...)
	}
	toBeVerified = append(toBeVerified, []byte(strconv.Itoa(int(header.GetBodySizeNumBytes())))...)
	toBeVerified = append(toBeVerified, body...)

	var digest []byte
	switch a.key.Curve.Params().BitSize {
	case 256:
		sum := sha256.Sum256(toBeVerified)
		digest = sum[:]
	case 384:
		sum := sha512.Sum384(toBeVerified)
		digest = sum[:]
	case 521:
		sum := sha512.Sum512(toBeVerified)
		digest = sum[:]
	default:
		return nil, fmt.Errorf("unsupported key size %d", a.key.Curve.Params().BitSize)
	}

	return ecdsa.SignASN1(rand.Reader, a.key, digest)
}

func (a *agent) request(ctx context.Context, msg *mmtp.MmtpMessage) (*mmtp.ResponseMessage, error) {
	ch := make(chan *mmtp.ResponseMessage, 1)
	a.pendingMu.Lock()
	a.pending[msg.GetUuid()] = ch
	a.pendingMu.Unlock()

	defer func() {
		a.pendingMu.Lock()
		delete(a.pending, msg.GetUuid())
		a.pendingMu.Unlock()
	}()

	if err := a.write(ctx, msg); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("timed out waiting for a response")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *agent) write(ctx context.Context, msg *mmtp.MmtpMessage) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("could not marshal the message: %w", err)
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.conn.Write(ctx, websocket.MessageBinary, data)
}

func (a *agent) readLoop(ctx context.Context) {
	for {
		kind, data, err := a.conn.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageBinary {
			continue
		}
		msg := &mmtp.MmtpMessage{}
		if err := proto.Unmarshal(data, msg); err != nil {
			continue
		}
		switch msg.GetMsgType() {
		case mmtp.MsgType_RESPONSE_MESSAGE:
			resp := msg.GetResponseMessage()
			a.pendingMu.Lock()
			ch, ok := a.pending[resp.GetResponseToUuid()]
			a.pendingMu.Unlock()
			if ok {
				select {
				case ch <- resp:
				default:
				}
			}
		case mmtp.MsgType_PROTOCOL_MESSAGE:
			protocol := msg.GetProtocolMessage()
			switch protocol.GetProtocolMsgType() {
			case mmtp.ProtocolMessageType_NOTIFY_MESSAGE:
				// The notification is answered with a receive request whose
				// response is read by this very loop, so it must not be
				// handled inline.
				notify := protocol.GetNotifyMessage()
				go a.handleNotify(context.Background(), notify)
			default:
				// Nothing else is expected outside of a response.
			}
		}
	}
}

func (a *agent) handleNotify(ctx context.Context, notify *mmtp.Notify) {
	metadata := notify.GetMessageMetadata()
	if len(metadata) == 0 {
		return
	}

	uuids := make([]string, 0, len(metadata))
	for _, entry := range metadata {
		uuids = append(uuids, entry.GetUuid())
	}
	a.notified += len(uuids)

	fmt.Printf(
		"[agent %s] notified about %d message(s): %s\n",
		a.mrn, len(uuids), strings.Join(uuids, ", "),
	)

	msg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_RECEIVE_MESSAGE,
				Body: &mmtp.ProtocolMessage_ReceiveMessage{
					ReceiveMessage: &mmtp.Receive{Filter: &mmtp.Filter{MessageUuids: uuids}},
				},
			},
		},
	}

	resp, err := a.request(ctx, msg)
	if err != nil {
		fmt.Printf("[agent %s] could not fetch the messages: %v\n", a.mrn, err)
		return
	}
	for _, content := range resp.GetMessageContent() {
		appMessage := content.GetMsg()
		if appMessage == nil {
			continue
		}
		a.received++
		summary, _ := json.Marshal(map[string]any{
			"subject":    appMessage.GetHeader().GetSubject(),
			"recipients": appMessage.GetHeader().GetRecipients().GetRecipients(),
			"sender":     appMessage.GetHeader().GetSender(),
			"body":       string(appMessage.GetBody()),
		})
		fmt.Printf("[agent %s] received %s\n", a.mrn, summary)
	}
}

func (a *agent) disconnect(ctx context.Context) {
	msg := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_PROTOCOL_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ProtocolMessage{
			ProtocolMessage: &mmtp.ProtocolMessage{
				ProtocolMsgType: mmtp.ProtocolMessageType_DISCONNECT_MESSAGE,
				Body: &mmtp.ProtocolMessage_DisconnectMessage{
					DisconnectMessage: &mmtp.Disconnect{},
				},
			},
		},
	}
	_ = a.write(ctx, msg)
	a.conn.Close(websocket.StatusNormalClosure, "done")
}

// -- helpers -------------------------------------------------------------

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}

	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ecdsaKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("the key in %s is a %T, only ECDSA keys are supported", path, key)
		}
		return ecdsaKey, nil
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("could not parse the key in %s: %w", path, err)
	}
	return key, nil
}

func loadCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func loadPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s holds no certificates", path)
	}
	return pool, nil
}

func mrnFromCert(cert *x509.Certificate) string {
	for _, name := range cert.Subject.Names {
		if name.Type.Equal(uidOID) {
			if value, ok := name.Value.(string); ok {
				return value
			}
		}
	}
	return ""
}
