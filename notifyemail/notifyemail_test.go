package notifyemail

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSMTP struct {
	addr     string
	mu       sync.Mutex
	messages []string
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeSMTP{addr: listener.Addr().String()}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.handle(conn)
		}
	}()
	return server
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	write := func(line string) {
		writer.WriteString(line + "\r\n")
		writer.Flush()
	}
	write("220 fake smtp ready")
	var data strings.Builder
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if inData {
			if strings.TrimRight(line, "\r\n") == "." {
				inData = false
				s.mu.Lock()
				s.messages = append(s.messages, data.String())
				s.mu.Unlock()
				data.Reset()
				write("250 OK")
				continue
			}
			data.WriteString(line)
			continue
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			write("250-fake")
			write("250 OK")
		case strings.HasPrefix(command, "MAIL FROM"), strings.HasPrefix(command, "RCPT TO"):
			write("250 OK")
		case strings.HasPrefix(command, "DATA"):
			inData = true
			write("354 send it")
		case strings.HasPrefix(command, "QUIT"):
			write("221 bye")
			return
		default:
			write("250 OK")
		}
	}
}

func (s *fakeSMTP) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return ""
	}
	return s.messages[len(s.messages)-1]
}

func configFor(t *testing.T, server *fakeSMTP) Config {
	t.Helper()
	host, portText, err := net.SplitHostPort(server.addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	return Config{
		Host: host,
		Port: port,
		TLS:  TLSNone,
		From: "notice@example.com",
	}
}

func TestSendDeliversUTF8Message(t *testing.T) {
	server := startFakeSMTP(t)
	cfg := configFor(t, server)
	if err := Send(context.Background(), cfg, "owner@example.com", "任务失败提醒", "原因：余额不够"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	raw := server.last()
	if raw == "" {
		t.Fatal("服务器没有收到邮件")
	}
	if !strings.Contains(raw, "To: owner@example.com") {
		t.Fatalf("收件人不对: %s", raw)
	}
	if !strings.Contains(raw, "Subject: =?UTF-8?b?") {
		t.Fatalf("中文主题应该按 RFC 2047 编码: %s", raw)
	}
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("邮件结构不对: %s", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		t.Fatalf("正文不是合法的 base64: %v", err)
	}
	if !strings.Contains(string(decoded), "原因：余额不够") {
		t.Fatalf("正文内容不对: %s", decoded)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		cfg  Config
		want bool
	}{
		{Config{Host: "", Port: 587, From: "a@example.com"}, false},
		{Config{Host: "smtp.example.com", Port: 0, From: "a@example.com"}, false},
		{Config{Host: "smtp.example.com", Port: 587, From: "not-an-email"}, false},
		{Config{Host: "smtp.example.com", Port: 587, From: "a@example.com"}, true},
	}
	for _, tc := range cases {
		ok := tc.cfg.Validate() == nil
		if ok != tc.want {
			t.Fatalf("%+v 的校验结果应该是 %v，实际 %v", tc.cfg, tc.want, ok)
		}
	}
}

func TestSendRejectsNewlineInSubject(t *testing.T) {
	server := startFakeSMTP(t)
	err := Send(context.Background(), configFor(t, server), "owner@example.com", "标题\r\nBcc: evil@example.com", "正文")
	if err == nil || !strings.Contains(err.Error(), "换行") {
		t.Fatalf("主题里的换行应该被拒绝: %v", err)
	}
}

func TestSendReportsConnectionError(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", Port: 1, TLS: TLSNone, From: "a@example.com", Timeout: time.Second}
	err := Send(context.Background(), cfg, "owner@example.com", "标题", "正文")
	if err == nil || !strings.Contains(err.Error(), "连不上邮件服务器") {
		t.Fatalf("连不上服务器时应该返回明确原因: %v", err)
	}
}
