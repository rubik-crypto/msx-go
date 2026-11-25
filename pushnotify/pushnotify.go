// Package pushnotify 把一条短消息推送到手机或群聊：支持 Bark、Server酱、推送加和自定义群机器人 Webhook。
//
// 四个通道都只用标准库的 HTTP 客户端；用户填写的 Webhook 地址会被限制为公网 https，
// 连接时还会检查解析出来的 IP，避免服务端被当成探测内网的跳板。
//
// 典型用法：
//
//	sender := pushnotify.New(nil)
//	err := sender.Send(ctx, pushnotify.Config{
//		Channel: pushnotify.Bark,
//		BarkKey: "https://api.day.app/你的密钥",
//	}, pushnotify.Message{Title: "标题", Body: "正文"})
package pushnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Channel 是推送通道。
type Channel string

const (
	// Bark 用于 iOS 上的 Bark App。
	Bark Channel = "bark"
	// ServerChan 用于 Server酱（推送到微信）。
	ServerChan Channel = "serverchan"
	// PushPlus 用于推送加（推送到微信服务号）。
	PushPlus Channel = "pushplus"
	// Webhook 用于企业微信、钉钉、飞书等群机器人。
	Webhook Channel = "webhook"
)

// Message 是一条待发送的提醒。标题和正文都应是普通文本。
type Message struct {
	Title string
	Body  string
}

// Config 是发送一条消息需要的配置。只有当前 Channel 对应的字段会被使用。
type Config struct {
	Channel       Channel
	BarkKey       string
	BarkGroup     string // 可选：Bark 通知分组名
	ServerChanKey string
	PushPlusToken string
	WebhookURL    string
}

// Configured 表示当前通道是否已经填好了必要的信息。
func (c Config) Configured() bool {
	switch c.Channel {
	case Bark:
		return strings.TrimSpace(c.BarkKey) != ""
	case ServerChan:
		return strings.TrimSpace(c.ServerChanKey) != ""
	case PushPlus:
		return strings.TrimSpace(c.PushPlusToken) != ""
	case Webhook:
		return strings.TrimSpace(c.WebhookURL) != ""
	default:
		return false
	}
}

// Validate 检查配置是否能直接用于发送。它不发起网络请求。
func (c Config) Validate() error {
	if !c.Configured() {
		return errors.New("推送配置不完整")
	}
	switch c.Channel {
	case Bark:
		_, _, err := ParseBarkTarget(c.BarkKey)
		return err
	case ServerChan:
		return ValidateServerChanKey(c.ServerChanKey)
	case PushPlus:
		return ValidatePushPlusToken(c.PushPlusToken)
	case Webhook:
		return ValidateWebhookURL(c.WebhookURL)
	default:
		return errors.New("不支持的推送通道")
	}
}

// Sender 负责发送消息。并发安全，可以复用。
type Sender struct {
	client *http.Client
}

// New 创建发送器。client 为 nil 时使用内置的安全客户端：
// 只允许 https、拒绝跳转到非公网地址，并在连接时拦截内网 IP。
func New(client *http.Client) *Sender {
	if client == nil {
		client = newSafeClient()
	}
	return &Sender{client: client}
}

// Send 按配置把消息发出去。返回的错误适合直接展示给用户，不包含密钥。
func (s *Sender) Send(ctx context.Context, cfg Config, msg Message) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(msg.Title) == "" && strings.TrimSpace(msg.Body) == "" {
		return errors.New("消息内容为空")
	}
	switch cfg.Channel {
	case Bark:
		return s.sendBark(ctx, cfg.BarkKey, cfg.BarkGroup, msg)
	case ServerChan:
		return s.sendServerChan(ctx, cfg.ServerChanKey, msg)
	case PushPlus:
		return s.sendPushPlus(ctx, cfg.PushPlusToken, msg)
	case Webhook:
		return s.sendWebhook(ctx, cfg.WebhookURL, msg)
	default:
		return errors.New("不支持的推送通道")
	}
}

func (s *Sender) sendBark(ctx context.Context, raw, group string, msg Message) error {
	base, key, err := ParseBarkTarget(raw)
	if err != nil {
		return err
	}
	body := map[string]any{
		"device_key": key,
		"title":      msg.Title,
		"body":       msg.Body,
	}
	if strings.TrimSpace(group) != "" {
		body["group"] = strings.TrimSpace(group)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return errors.New("消息内容整理失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/push", bytes.NewReader(payload))
	if err != nil {
		return errors.New("Bark 地址格式不正确")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	out, err := s.do(req)
	if err != nil {
		return err
	}
	return responseError(out, []string{"code"}, 200)
}

func (s *Sender) sendServerChan(ctx context.Context, raw string, msg Message) error {
	key := strings.TrimSpace(raw)
	if err := ValidateServerChanKey(key); err != nil {
		return err
	}
	form := url.Values{}
	form.Set("title", msg.Title)
	form.Set("desp", msg.Body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://sctapi.ftqq.com/"+url.PathEscape(key)+".send", strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("Server酱地址格式不正确")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	out, err := s.do(req)
	if err != nil {
		return err
	}
	return responseError(out, []string{"code"}, 0)
}

func (s *Sender) sendPushPlus(ctx context.Context, raw string, msg Message) error {
	token := strings.TrimSpace(raw)
	if err := ValidatePushPlusToken(token); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{
		"token":    token,
		"title":    msg.Title,
		"content":  msg.Body,
		"template": "txt",
	})
	if err != nil {
		return errors.New("消息内容整理失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.pushplus.plus/send", bytes.NewReader(payload))
	if err != nil {
		return errors.New("推送加地址格式不正确")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	out, err := s.do(req)
	if err != nil {
		return err
	}
	// 推送加用 code=200 表示成功，和别的通道用 0 不一样。
	return responseError(out, []string{"code"}, 200)
}

func (s *Sender) sendWebhook(ctx context.Context, raw string, msg Message) error {
	if err := ValidateWebhookURL(raw); err != nil {
		return err
	}
	text := msg.Title + "\n" + msg.Body
	// 同时带企微/钉钉（msgtype + text.content）和飞书（msg_type + content.text）的字段，
	// 三种群机器人都会读到属于自己的那一份，其余字段会被忽略。
	payload, err := json.Marshal(map[string]any{
		"msgtype":  "text",
		"text":     map[string]any{"content": text},
		"msg_type": "text",
		"content":  map[string]any{"text": text},
		"title":    msg.Title,
		"body":     msg.Body,
	})
	if err != nil {
		return errors.New("消息内容整理失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(raw), bytes.NewReader(payload))
	if err != nil {
		return errors.New("Webhook 地址格式不正确")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	out, err := s.do(req)
	if err != nil {
		return err
	}
	return responseError(out, []string{"errcode", "code"}, 0)
}

func (s *Sender) do(req *http.Request) (map[string]any, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, errors.New("推送超时，请检查网络后重试")
		}
		return nil, errors.New("连不上推送服务，请检查网络或推送地址")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return nil, errors.New("推送服务返回内容读取失败")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("推送服务拒绝了密钥，请检查是否填写正确")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errors.New("推送服务请求太频繁，请稍后再试")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("推送服务返回错误（%d），请稍后再试", resp.StatusCode)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil
	}
	return payload, nil
}

func responseError(payload map[string]any, codeKeys []string, success float64) error {
	if payload == nil {
		return nil
	}
	for _, key := range codeKeys {
		n, ok := numberField(payload, key)
		if !ok || n == success {
			continue
		}
		msg := ""
		for _, msgKey := range []string{"message", "msg", "errmsg", "error"} {
			if v, ok := payload[msgKey].(string); ok && strings.TrimSpace(v) != "" {
				msg = strings.TrimSpace(v)
				break
			}
		}
		if msg == "" {
			return fmt.Errorf("推送服务没有接受这条消息（错误码 %v）", n)
		}
		return fmt.Errorf("推送服务没有接受这条消息：%s", truncateRunes(msg, 120))
	}
	return nil
}

func numberField(payload map[string]any, key string) (float64, bool) {
	v, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

// ParseBarkTarget 从用户输入里解析出 Bark 服务地址和密钥。
// 可以传完整的推送地址（https://api.day.app/密钥/...），也可以只传密钥。
func ParseBarkTarget(raw string) (baseURL, key string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", errors.New("请先填写 Bark 推送地址")
	}
	if strings.Contains(s, "://") {
		u, parseErr := url.Parse(s)
		if parseErr != nil || u.Host == "" {
			return "", "", errors.New("Bark 推送地址格式不正确")
		}
		if !strings.EqualFold(u.Scheme, "https") {
			return "", "", errors.New("Bark 推送地址需要以 https:// 开头")
		}
		if u.User != nil {
			return "", "", errors.New("Bark 推送地址里不要带账号密码")
		}
		if err := validatePublicHost(u.Hostname()); err != nil {
			return "", "", err
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) > 0 {
			key = strings.TrimSpace(parts[0])
		}
		if key == "" {
			return "", "", errors.New("地址里没有找到 Bark 推送密钥")
		}
		return "https://" + u.Host, key, nil
	}
	if strings.ContainsAny(s, " /\\?#") {
		return "", "", errors.New("Bark 密钥格式不正确")
	}
	return "https://api.day.app", s, nil
}

// ValidateServerChanKey 检查 Server酱 SendKey 的基本格式。
func ValidateServerChanKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("请先填写 Server酱 SendKey")
	}
	if strings.ContainsAny(key, " /\\?#&") {
		return errors.New("Server酱 SendKey 格式不正确")
	}
	return nil
}

// ValidatePushPlusToken 检查推送加 token 的基本格式。
func ValidatePushPlusToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("请先填写推送加 token")
	}
	if strings.ContainsAny(token, " /\\?#&") {
		return errors.New("推送加 token 格式不正确")
	}
	return nil
}

// ValidateWebhookURL 只接受公网 https 地址，并拒绝本机、内网和带账号密码的地址。
func ValidateWebhookURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return errors.New("请先填写 Webhook 地址")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return errors.New("Webhook 地址格式不正确")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errors.New("为了安全，Webhook 地址需要以 https:// 开头")
	}
	if u.User != nil {
		return errors.New("Webhook 地址里不要带账号密码")
	}
	return validatePublicHost(u.Hostname())
}

func validatePublicHost(host string) error {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return errors.New("推送地址里没有域名")
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return errors.New("推送地址不能指向本机或内网")
	}
	if ip := net.ParseIP(h); ip != nil && isBlockedIP(ip) {
		return errors.New("推送地址不能指向本机或内网")
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	// 运营商级 NAT 地址段 100.64.0.0/10 也不应该从公网服务访问。
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

func newSafeClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 8 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
				return errors.New("推送地址指向内网，已拒绝连接")
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("推送地址跳转次数太多")
			}
			u := req.URL
			if !strings.EqualFold(u.Scheme, "https") {
				return errors.New("推送地址只能以 https:// 开头")
			}
			if u.User != nil {
				return errors.New("推送地址里不要带账号密码")
			}
			return validatePublicHost(u.Hostname())
		},
	}
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
