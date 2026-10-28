// mailclient 是 mail gateway 的簡易 client：以 mTLS 連線，呼叫 SendMail（同步）或 QueueMail（非同步）
// 寄出一封信。用來確認 gateway 的設定、憑證與實際寄送結果，也可以當作呼叫端串接的範例。
//
// 憑證資料夾即解開的 <CN>.tar.gz：client.crt、client.key、ca.crt。
//
//	mailclient -addr 10.0.0.5:20050 -certs certs -to user@example.com
//	mailclient -addr 10.0.0.5:20050 -to user@example.com -subject 測試 -attach 報價單.pdf -queue
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	api "maild/pkg/grpc/mail"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// stringList 為可重複指定的 flag，例如 -attach a.pdf -attach b.png。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:20050", "gateway address (host:port)")
	certDir := flag.String("certs", "certs", "directory containing client.crt, client.key and ca.crt (the extracted <CN>.tar.gz)")
	serverName := flag.String("server-name", "", "name to verify the gateway certificate against (default: host of -addr)")
	to := flag.String("to", "", "recipients, comma separated (required)")
	sender := flag.String("sender", "", "sender alias (default: the client's default sender)")
	senderName := flag.String("sender-name", "", "sender display name (default: the sender's configured name)")
	subject := flag.String("subject", "Mail Gateway 測試信", "subject")
	body := flag.String("body", "", "plain text body (default: a test message with the current time)")
	htmlBody := flag.String("html", "", "HTML body")
	queue := flag.Bool("queue", false, "use QueueMail (asynchronous) instead of SendMail")
	timeout := flag.Duration("timeout", 60*time.Second, "RPC timeout")
	var attachments stringList
	flag.Var(&attachments, "attach", "attachment file; repeat for multiple files")
	flag.Parse()

	if *to == "" {
		fmt.Fprintln(os.Stderr, "-to is required")
		flag.Usage()
		os.Exit(2)
	}
	if *body == "" && *htmlBody == "" {
		*body = "這是一封由 Mail Gateway 寄出的測試信。\n寄出時間：" + time.Now().Format(time.RFC3339)
	}

	mail := &api.Mail{
		Sender:     *sender,
		SenderName: *senderName,
		Subject:    *subject,
		TextBody:   *body,
		HtmlBody:   *htmlBody,
		Labels:     map[string]string{"source": "mailclient"},
	}
	for a := range strings.SplitSeq(*to, ",") {
		if a = strings.TrimSpace(a); a != "" {
			mail.To = append(mail.To, &api.Address{Address: a})
		}
	}
	for _, path := range attachments {
		content, err := os.ReadFile(path)
		if err != nil {
			exitf("read attachment failed: %v", err)
		}
		// content_type 留空，由 gateway 依副檔名推斷。
		mail.Attachments = append(mail.Attachments, &api.Attachment{Filename: filepath.Base(path), Content: content})
	}

	conn, err := dial(*addr, *certDir, *serverName)
	if err != nil {
		exitf("%v", err)
	}
	defer conn.Close()
	client := api.NewMailAPIClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *queue {
		resp, err := client.QueueMail(ctx, &api.QueueMailRequest{Mail: mail})
		if err != nil {
			exitRPC("QueueMail", err)
		}
		fmt.Println("queued (the gateway sends it in the background; check the gateway log for the result)")
		fmt.Println("  mail_id:          ", resp.MailId)
		fmt.Println("  header_message_id:", resp.HeaderMessageId)
		return
	}

	resp, err := client.SendMail(ctx, &api.SendMailRequest{Mail: mail})
	if err != nil {
		exitRPC("SendMail", err)
	}
	fmt.Println("sent")
	fmt.Println("  mail_id:          ", resp.MailId)
	fmt.Println("  header_message_id:", resp.HeaderMessageId)
	fmt.Println("  smtp_response:    ", resp.SmtpResponse)
	fmt.Println("  sent_at:          ", resp.SentAt.AsTime().Local().Format(time.RFC3339))
}

// dial 以 certDir 裡的 client 憑證建立 mTLS 連線。serverName 留空時以 addr 的 host 驗證 gateway 憑證。
func dial(addr, certDir, serverName string) (*grpc.ClientConn, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "client.crt"), filepath.Join(certDir, "client.key"))
	if err != nil {
		return nil, fmt.Errorf("load client certificate failed: %w", err)
	}
	caFile := filepath.Join(certDir, "ca.crt")
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate failed: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificate found in %s", caFile)
	}
	creds := credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		ServerName:   serverName,
		Certificates: []tls.Certificate{cert},
	})
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

func exitRPC(rpc string, err error) {
	st := status.Convert(err)
	exitf("%s failed: %s: %s", rpc, st.Code(), st.Message())
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
