// Package smtptest 提供測試用的最小 SMTP server（不加密），回應可依指令覆寫。
package smtptest

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
)

// Server 為測試用 SMTP server。指令名稱：EHLO、AUTH、MAIL、RCPT:<地址>、DATA、END（DATA 結尾）。
type Server struct {
	ln       net.Listener
	mu       sync.Mutex
	replies  map[string]string
	hangEnd  bool
	messages []Message
}

// Message 為 server 收下的一封郵件。
type Message struct {
	From       string
	Recipients []string
	Data       string // 已還原 dot-stuffing
}

// NewServer 在 127.0.0.1 的隨機 port 啟動 server，測試結束時自動關閉。
func NewServer(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{ln: ln, replies: map[string]string{}}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close 關閉 listener，之後的連線會被拒絕。
func (s *Server) Close() { s.ln.Close() }

// Reply 覆寫某個指令的回應，例如 Reply("END", "550 5.4.5 quota exceeded")。
func (s *Server) Reply(cmd, resp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies[cmd] = resp
}

// HangAfterData 設定收到 DATA 結尾後不回應，模擬等待回應時逾時。
func (s *Server) HangAfterData() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hangEnd = true
}

// Messages 回傳目前收下的郵件。
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

func (s *Server) reply(cmd, fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.replies[cmd]; ok {
		return r
	}
	return fallback
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(line string) { conn.Write([]byte(line + "\r\n")) }

	var msg Message
	write("220 smtptest ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			write(s.reply("EHLO", "250-smtptest\r\n250 AUTH PLAIN"))
		case "AUTH":
			write(s.reply("AUTH", "235 2.7.0 Accepted"))
		case "MAIL":
			msg = Message{From: angleAddr(line)}
			write(s.reply("MAIL", "250 2.1.0 OK"))
		case "RCPT":
			addr := angleAddr(line)
			resp := s.reply("RCPT:"+addr, "250 2.1.5 OK")
			if strings.HasPrefix(resp, "250") {
				msg.Recipients = append(msg.Recipients, addr)
			}
			write(resp)
		case "DATA":
			resp := s.reply("DATA", "354 go ahead")
			write(resp)
			if !strings.HasPrefix(resp, "354") {
				continue
			}
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			msg.Data = data.String()
			s.mu.Lock()
			hang := s.hangEnd
			s.mu.Unlock()
			if hang {
				r.ReadString('\n') // 不回應，等 client 自己逾時斷線
				return
			}
			resp = s.reply("END", "250 2.0.0 OK queued as 4A1B2C")
			if strings.HasPrefix(resp, "250") {
				s.mu.Lock()
				s.messages = append(s.messages, msg)
				s.mu.Unlock()
			}
			write(resp)
		case "RSET":
			msg = Message{}
			write("250 2.0.0 OK")
		case "QUIT":
			write("221 bye")
			return
		default:
			write("502 unknown command")
		}
	}
}

// angleAddr 取出 MAIL FROM:<x> 或 RCPT TO:<x> 裡的地址。
func angleAddr(line string) string {
	start := strings.IndexByte(line, '<')
	end := strings.IndexByte(line, '>')
	if start < 0 || end < start {
		return ""
	}
	return line[start+1 : end]
}
