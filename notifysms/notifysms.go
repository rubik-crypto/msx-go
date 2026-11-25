// Package notifysms 通过短信服务发送一条短消息：支持阿里云短信和通用 HTTP 网关。
//
// 阿里云通道走官方的 SendSms 接口（RPC 签名，HMAC-SHA1），只需要 AccessKey、
// 已审核的短信签名和模板编号；通用网关通道用 {phone} 和 {text} 两个占位符
// 拼出 GET 地址，适合对接自建或第三方的短信转发服务。
//
// 通用网关默认只允许公网 https 地址，并在建立连接时拦截内网 IP，避免服务端
// 被当成探测内网的跳板；确实要连内网网关时，把 GatewayConfig.AllowPrivate 打开。
//
// 典型用法：
//
//	err := notifysms.SendAliyun(ctx, notifysms.AliyunConfig{
//		AccessKeyID:     "…",
//		AccessKeySecret: "…",
//		SignName:        "某某通知",
//		TemplateCode:    "SMS_123456",
//	}, "13800138000", "任务执行失败：余额不足")
package notifysms

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"
)

// DefaultAliyunEndpoint 是阿里云短信的默认接口地址。
const DefaultAliyunEndpoint = "https://dysmsapi.aliyuncs.com/"

// AliyunConfig 是阿里云短信的配置。
type AliyunConfig struct {
	AccessKeyID     string
	AccessKeySecret string
	// SignName 是短信签名，例如「某某通知」，必须在控制台审核通过。
	SignName string
	// TemplateCode 是模板编号，例如 SMS_123456。
	TemplateCode string
	// TemplateParam 是模板里放正文的变量名，留空按 content 处理。
	TemplateParam string
	// Endpoint 可以覆盖默认接口地址，方便测试或走代理。
	Endpoint string
	// Timeout 是单次请求的超时，0 表示 15 秒。
	Timeout time.Duration
}

// Validate 检查阿里云短信配置是否完整。
func (c AliyunConfig) Validate() error {
	if strings.TrimSpace(c.AccessKeyID) == "" || strings.TrimSpace(c.AccessKeySecret) == "" {
		return errors.New("阿里云短信的 AccessKey 没有填写完整")
	}
	if strings.TrimSpace(c.SignName) == "" {
		return errors.New("短信签名是空的")
	}
	if strings.TrimSpace(c.TemplateCode) == "" {
		return errors.New("短信模板编号是空的")
	}
	return nil
}

func (c AliyunConfig) endpoint() string {
	value := strings.TrimSpace(c.Endpoint)
	if value == "" {
		return DefaultAliyunEndpoint
	}
	if !strings.HasSuffix(value, "/") {
		value += "/"
	}
	return value
}

func (c AliyunConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

func (c AliyunConfig) paramName() string {
	value := strings.TrimSpace(c.TemplateParam)
	if value == "" {
		return "content"
	}
	return value
}

// SendAliyun 用阿里云短信的 SendSms 接口发一条短信。
func SendAliyun(ctx context.Context, cfg AliyunConfig, phone, text string) error {
	return sendAliyunAt(ctx, cfg, phone, text, time.Now(), nil)
}

// sendAliyunAt 把当前时间和 HTTP 客户端抽出来，方便测试固定签名参数。
func sendAliyunAt(ctx context.Context, cfg AliyunConfig, phone, text string, now time.Time, client *http.Client) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("短信内容是空的")
	}
	templateParam, err := json.Marshal(map[string]string{cfg.paramName(): text})
	if err != nil {
		return errors.New("短信内容整理失败")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("生成短信请求失败")
	}
	params := map[string]string{
		"AccessKeyId":      cfg.AccessKeyID,
		"Action":           "SendSms",
		"Format":           "JSON",
		"PhoneNumbers":     normalized,
		"RegionId":         "cn-hangzhou",
		"SignName":         cfg.SignName,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   hex.EncodeToString(nonce),
		"SignatureVersion": "1.0",
		"TemplateCode":     cfg.TemplateCode,
		"TemplateParam":    string(templateParam),
		"Timestamp":        now.UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          "2017-05-25",
	}
	canonical := canonicalQuery(params)
	stringToSign := "GET&%2F&" + percentEncode(canonical)
	mac := hmac.New(sha1.New, []byte(cfg.AccessKeySecret+"&"))
	mac.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	endpoint := cfg.endpoint() + "?Signature=" + percentEncode(signature) + "&" + canonical

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("短信请求生成失败")
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.timeout()}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("发送短信超时，请稍后再试")
		}
		return errors.New("连不上短信服务，请检查网络")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var result struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	_ = json.Unmarshal(body, &result)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("短信服务返回错误（%d）", resp.StatusCode)
	}
	if !strings.EqualFold(result.Code, "OK") {
		message := truncateRunes(result.Message, 120)
		if message == "" {
			message = "未知原因"
		}
		return fmt.Errorf("短信没有发出去：%s（%s）", message, result.Code)
	}
	return nil
}

// GatewayConfig 是通用短信网关的配置。
type GatewayConfig struct {
	// URLTemplate 是网关地址，用 {phone} 和 {text} 做占位符。
	URLTemplate string
	// AllowPrivate 允许连接本机和内网地址，只建议在测试或自建内网网关时打开。
	AllowPrivate bool
	// Timeout 是单次请求的超时，0 表示 15 秒。
	Timeout time.Duration
}

func (c GatewayConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// SendGateway 按模板拼出地址并发起 GET 请求，返回 2xx 视为发送成功。
func SendGateway(ctx context.Context, cfg GatewayConfig, phone, text string) error {
	template := strings.TrimSpace(cfg.URLTemplate)
	if template == "" {
		return errors.New("短信网关地址是空的")
	}
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return err
	}
	target := strings.ReplaceAll(template, "{phone}", url.QueryEscape(normalized))
	target = strings.ReplaceAll(target, "{text}", url.QueryEscape(strings.TrimSpace(text)))
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return errors.New("短信网关地址格式不正确")
	}
	if parsed.User != nil {
		return errors.New("短信网关地址里不要带账号密码")
	}
	if !cfg.AllowPrivate {
		if !strings.EqualFold(parsed.Scheme, "https") {
			return errors.New("为了安全，短信网关地址需要以 https:// 开头")
		}
		if err := validatePublicHost(parsed.Hostname()); err != nil {
			return err
		}
	} else if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return errors.New("短信网关地址要以 http:// 或 https:// 开头")
	}

	client := &http.Client{Timeout: cfg.timeout()}
	if !cfg.AllowPrivate {
		client = newSafeClient(cfg.timeout())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("短信请求生成失败")
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("发送短信超时，请稍后再试")
		}
		return errors.New("连不上短信网关，请检查地址和网络")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("短信网关返回错误（%d）", resp.StatusCode)
	}
	return nil
}

// NormalizePhone 去掉手机号里的空格、横线和括号，保留开头的一个 +。
func NormalizePhone(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", errors.New("手机号是空的")
	}
	var out strings.Builder
	for i, r := range text {
		switch {
		case r == '+' && i == 0:
			out.WriteRune(r)
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
			continue
		default:
			return "", errors.New("手机号只能填数字，可以带开头的 +86")
		}
	}
	digits := strings.TrimPrefix(out.String(), "+")
	if len(digits) < 6 || len(digits) > 20 {
		return "", errors.New("手机号长度不对，请检查后再填")
	}
	return out.String(), nil
}

// canonicalQuery 按阿里云 RPC 签名规则拼参数：按键名排序，键和值都做百分号编码。
func canonicalQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, percentEncode(key)+"="+percentEncode(params[key]))
	}
	return strings.Join(parts, "&")
}

// percentEncode 是 RFC 3986 编码：空格写成 %20，只有字母、数字和 -_.~ 不编码。
func percentEncode(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteString(fmt.Sprintf("%02X", c))
	}
	return out.String()
}

// validatePublicHost 拒绝本机、内网和带账号密码的网关地址。
func validatePublicHost(host string) error {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return errors.New("短信网关地址里没有域名")
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return errors.New("短信网关地址不能指向本机或内网")
	}
	if ip := net.ParseIP(h); ip != nil && isBlockedIP(ip) {
		return errors.New("短信网关地址不能指向本机或内网")
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
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// newSafeClient 在真正建立连接时再查一次 IP，防止域名解析到内网（DNS 重绑定）。
func newSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 8 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
				return errors.New("短信网关指向内网，已拒绝连接")
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("短信网关跳转次数太多")
			}
			if !strings.EqualFold(req.URL.Scheme, "https") || req.URL.User != nil {
				return errors.New("短信网关只能跳转到公网 https 地址")
			}
			return validatePublicHost(req.URL.Hostname())
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
