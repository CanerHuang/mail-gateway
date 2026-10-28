package mailServer_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mailServer "maild/internal/api/mailServer"
	"maild/internal/config"
	"maild/internal/service"
	"maild/internal/smtptest"
	api "maild/pkg/grpc/mail"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// testCA 是測試用的 CA，可以簽 server 與 client 憑證。
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue 簽發一張憑證，回傳 PEM 格式的憑證與私鑰。
func (ca *testCA) issue(t *testing.T, cn string, usage x509.ExtKeyUsage, notAfter time.Time, ips ...net.IP) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func (ca *testCA) clientCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	certPEM, keyPEM := ca.issue(t, cn, x509.ExtKeyUsageClientAuth, time.Now().Add(24*time.Hour))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

type gateway struct {
	addr string
	ca   *testCA
	smtp *smtptest.Server
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// startGateway 以測試憑證與假的 SMTP server 啟動完整的 gateway（gRPC server + 背景寄送）。
func startGateway(t *testing.T) *gateway {
	t.Helper()
	dir := t.TempDir()
	ca := newCA(t, "test-ca")
	serverCert, serverKey := ca.issue(t, "maild", x509.ExtKeyUsageServerAuth, time.Now().Add(24*time.Hour), net.IPv4(127, 0, 0, 1))
	writeFile(t, filepath.Join(dir, "server.crt"), serverCert)
	writeFile(t, filepath.Join(dir, "server.key"), serverKey)
	writeFile(t, filepath.Join(dir, "ca.crt"), ca.pem)

	smtp := smtptest.NewServer(t)
	configPath := filepath.Join(dir, "maild.toml")
	writeFile(t, configPath, fmt.Appendf(nil, `
[server]
listen_address = '127.0.0.1:0'
connection_timeout = '5s'
shutdown_timeout = '5s'
max_request_size_mib = 4

[server.keepalive]
time = '10s'
timeout = '3s'
min_time = '5s'
max_connection_age = '1h'
max_connection_age_grace = '5s'

[tls]
cert_file = '%[1]s/server.crt'
key_file = '%[1]s/server.key'
client_ca_file = '%[1]s/ca.crt'
expiry_warning = '720h'

[smtp]
timeout = '2s'
max_sync_connections = 2
sender_pause = '1h'

[queue]
spool_dir = '%[1]s/spool'
scan_interval = '1m'
workers = 1
max_attempts = 3
retry_initial_interval = '1m'
retry_max_interval = '1h'
max_queued_mails = 10
max_schedule_ahead = '168h'

[mail]
max_recipients = 10
max_attachments_size_mib = 2
max_labels = 5
max_label_key_length = 64
max_label_value_length = 256

[log]
max_field_length = 200

[senders.service]
address = 'service@example.com'
name = 'Example Service'

[senders.service.smtp]
host = '127.0.0.1'
port = %[2]d
security = 'none'
username = ''
password = ''

[clients.order-service]
senders = ['service']
default_sender = 'service'
rate_limit = 0
rate_limit_window = '1h'
`, dir, smtp.Port()))

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	tlsManager, err := mailServer.NewTLSManager(cfg.TLS)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	queueDone := make(chan struct{})
	go func() {
		svc.RunQueue(ctx)
		close(queueDone)
	}()
	server, err := mailServer.NewServer(svc, cfg.Server, tlsManager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		server.Stop()
		<-queueDone
	})
	return &gateway{addr: server.Addr().String(), ca: ca, smtp: smtp}
}

func (g *gateway) dial(t *testing.T, cert tls.Certificate, maxVersion uint16) api.MailAPIClient {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(g.ca.pem)
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MaxVersion:   maxVersion,
	})
	conn, err := grpc.NewClient(g.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return api.NewMailAPIClient(conn)
}

func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testMail() *api.Mail {
	return &api.Mail{
		To:       []*api.Address{{Address: "a@example.com", Name: "王小明"}},
		Subject:  "訂單已出貨",
		TextBody: "hello",
		Attachments: []*api.Attachment{
			{Filename: "報價單.pdf", Content: []byte("%PDF-1.4 test")},
		},
		Labels: map[string]string{"order_id": "A1234"},
	}
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestGateway_SendMail(t *testing.T) {
	g := startGateway(t)
	client := g.dial(t, g.ca.clientCert(t, "order-service"), 0)

	resp, err := client.SendMail(callCtx(t), &api.SendMailRequest{Mail: testMail()})
	if err != nil {
		t.Fatalf("SendMail: %v", err)
	}
	if resp.GetHeaderMessageId() != resp.GetMailId()+"@example.com" {
		t.Errorf("header_message_id = %q (mail_id %q); should be mail_id@sender domain without <>", resp.GetHeaderMessageId(), resp.GetMailId())
	}
	if resp.GetSmtpResponse() != "250 2.0.0 OK queued as 4A1B2C" || resp.GetSentAt() == nil {
		t.Errorf("response = %+v", resp)
	}
	msgs := g.smtp.Messages()
	if len(msgs) != 1 || msgs[0].From != "service@example.com" || !strings.Contains(msgs[0].Data, "<"+resp.GetHeaderMessageId()+">") {
		t.Fatalf("SMTP received %+v", msgs)
	}
}

func TestGateway_QueueMail(t *testing.T) {
	g := startGateway(t)
	client := g.dial(t, g.ca.clientCert(t, "order-service"), 0)

	resp, err := client.QueueMail(callCtx(t), &api.QueueMailRequest{Mail: testMail()})
	if err != nil {
		t.Fatalf("QueueMail: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(g.smtp.Messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	msgs := g.smtp.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Data, resp.GetHeaderMessageId()) {
		t.Fatalf("queued mail should be delivered in the background, SMTP received %+v", msgs)
	}
}

func TestGateway_ClientIdentity(t *testing.T) {
	g := startGateway(t)

	// 憑證有效但 CN 不在名單上：VersionGet 可以呼叫，其他 RPC 回 PermissionDenied。
	stranger := g.dial(t, g.ca.clientCert(t, "stranger"), 0)
	if _, err := stranger.VersionGet(callCtx(t), &emptypb.Empty{}); err != nil {
		t.Fatalf("VersionGet should only require mTLS: %v", err)
	}
	_, err := stranger.ListSenders(callCtx(t), &emptypb.Empty{})
	wantCode(t, err, codes.PermissionDenied)

	// 別的 CA 簽的憑證：TLS 握手就失敗，請求到不了 gateway。
	other := g.dial(t, newCA(t, "other-ca").clientCert(t, "order-service"), 0)
	_, err = other.VersionGet(callCtx(t), &emptypb.Empty{})
	wantCode(t, err, codes.Unavailable)

	// 只接受 TLS 1.3。
	tls12 := g.dial(t, g.ca.clientCert(t, "order-service"), tls.VersionTLS12)
	_, err = tls12.VersionGet(callCtx(t), &emptypb.Empty{})
	wantCode(t, err, codes.Unavailable)
}

func TestGateway_ErrorCodes(t *testing.T) {
	g := startGateway(t)
	client := g.dial(t, g.ca.clientCert(t, "order-service"), 0)

	senders, err := client.ListSenders(callCtx(t), &emptypb.Empty{})
	if err != nil || len(senders.GetSenders()) != 1 || senders.GetSenders()[0].GetAlias() != "service" || !senders.GetSenders()[0].GetIsDefault() {
		t.Fatalf("ListSenders = %v, %v", senders, err)
	}

	noRecipient := testMail()
	noRecipient.To = nil
	_, err = client.SendMail(callCtx(t), &api.SendMailRequest{Mail: noRecipient})
	wantCode(t, err, codes.InvalidArgument)

	otherSender := testMail()
	otherSender.Sender = "billing"
	_, err = client.QueueMail(callCtx(t), &api.QueueMailRequest{Mail: otherSender})
	wantCode(t, err, codes.PermissionDenied)

	g.smtp.Reply("END", "550 5.1.1 user unknown")
	_, err = client.SendMail(callCtx(t), &api.SendMailRequest{Mail: testMail()})
	wantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(status.Convert(err).Message(), "550 5.1.1 user unknown") {
		t.Errorf("message should include the SMTP response: %q", status.Convert(err).Message())
	}

	g.smtp.Reply("END", "451 4.3.0 try again later")
	_, err = client.SendMail(callCtx(t), &api.SendMailRequest{Mail: testMail()})
	wantCode(t, err, codes.Unavailable)

	g.smtp.Reply("END", "550 5.4.5 Daily user sending quota exceeded.")
	_, err = client.SendMail(callCtx(t), &api.SendMailRequest{Mail: testMail()})
	wantCode(t, err, codes.ResourceExhausted)
	if msg := status.Convert(err).Message(); !strings.HasPrefix(msg, "sender daily quota exceeded") {
		t.Errorf("quota message should start with the fixed text: %q", msg)
	}
}
