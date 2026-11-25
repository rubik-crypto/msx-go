// Package notifyemail 通过 SMTP 服务器发送纯文本通知邮件，支持 STARTTLS、SSL 和明文三种连接方式。
//
// 邮件主题和正文都用 UTF-8，主题按 RFC 2047 编码、正文按 base64 传输，
// 中文在常见邮箱客户端里不会变成乱码。包内只用标准库。
//
// 典型用法：
//
//	err := notifyemail.Send(context.Background(), notifyemail.Config{
//		Host: "smtp.example.com",
//		Port: 587,
//		TLS:  notifyemail.TLSStartTLS,
//		User: "notice@example.com",
//		Pass: "授权码",
//		From: "notice@example.com",
//	}, "owner@example.com", "任务失败", "原因：余额不足")
package notifyemail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// 连接方式。TLSStartTLS 适合 587 端口，TLSSSL 适合 465 端口，
// TLSNone 只建议在内网或本机的中继上使用。
const (
	TLSStartTLS = "starttls"
	TLSSSL      = "ssl"
	TLSNone     = "none"
)

// 默认连接超时，避免邮件服务器不回应时把调用方一直挂住。
const defaultTimeout = 20 * time.Second

// Config 是一个 SMTP 发信配置。
type Config struct {
	Host string
	Port int
	// TLS 是连接方式：starttls / ssl / none，留空按 starttls 处理。
	TLS  string
	User string
	Pass string
	// From 是发件地址，一般是和登录账号一致的完整邮箱。
	From string
	// Timeout 是连接和读写的超时，0 表示使用默认的 20 秒。
	Timeout time.Duration
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

func (c Config) tlsMode() string {
	switch strings.ToLower(strings.TrimSpace(c.TLS)) {
	case TLSSSL:
		return TLSSSL
	case TLSNone:
		return TLSNone
	default:
		return TLSStartTLS
	}
}

// Validate 检查配置够不够发一封邮件。
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return errors.New("邮件服务器地址是空的")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("邮件服务器端口不对")
	}
	if !looksLikeEmail(c.From) {
		return errors.New("发件邮箱格式不正确")
	}
	return nil
}

// looksLikeEmail 只做最基本的格式检查，不验证邮箱是否真实存在。
func looksLikeEmail(value string) bool {
	value = strings.TrimSpace(value)
	at := strings.IndexByte(value, '@')
	if at <= 0 || at == len(value)-1 || strings.ContainsAny(value, " \t\r\n<>,;") {
		return false
	}
	domain := value[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

// Send 发送一封纯文本邮件。
func Send(ctx context.Context, cfg Config, to, subject, body string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	to = strings.TrimSpace(to)
	if !looksLikeEmail(to) {
		return errors.New("收件邮箱格式不正确")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	message, err := buildMessage(cfg.From, to, subject, body)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: cfg.timeout()}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("连不上邮件服务器：%w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(cfg.timeout()))
	}

	if cfg.tlsMode() == TLSSSL {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("邮件服务器加密连接失败：%w", err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("邮件服务器握手失败：%w", err)
	}
	defer client.Close()
	if cfg.tlsMode() == TLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{
				ServerName: cfg.Host,
				MinVersion: tls.VersionTLS12,
			}); err != nil {
				return fmt.Errorf("邮件服务器加密连接失败：%w", err)
			}
		}
	}
	if cfg.User != "" {
		auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("邮箱账号或授权码不正确：%w", err)
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("发件地址被服务器拒绝：%w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("收件邮箱被服务器拒绝：%w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("邮件内容发送失败：%w", err)
	}
	if _, err := writer.Write([]byte(message)); err != nil {
		writer.Close()
		return fmt.Errorf("邮件内容发送失败：%w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("邮件内容发送失败：%w", err)
	}
	return client.Quit()
}

// buildMessage 拼出完整的邮件内容。主题里有中文时必须按 RFC 2047 编码，
// 正文用 base64，避免不同邮件服务器对 8bit 内容的处理不一致。
func buildMessage(from, to, subject, body string) (string, error) {
	if strings.ContainsAny(subject, "\r\n") {
		return "", errors.New("邮件主题不能包含换行")
	}
	var out strings.Builder
	out.WriteString("From: " + from + "\r\n")
	out.WriteString("To: " + to + "\r\n")
	out.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", subject) + "\r\n")
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	out.WriteString("Content-Transfer-Encoding: base64\r\n")
	out.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	out.WriteString("\r\n")
	out.WriteString(base64.StdEncoding.EncodeToString([]byte(body)))
	out.WriteString("\r\n")
	return out.String(), nil
}
