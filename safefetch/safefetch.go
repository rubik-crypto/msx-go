// Package safefetch 安全地抓取用户填写的网址：只允许公网地址、限制响应大小和超时，并自动把网页编码转成 UTF-8。
//
// 这不是一个普通 HTTP 客户端。只要服务端会去访问「用户填进来的网址」（网页监控、链接预览、
// 抓标题、远程图片代理、SEO 检查这类功能），就应该走这里，避免用户把服务当成探测内网、
// 访问本机管理口的跳板。跳转的每一跳都会重新校验，连接建立时还会检查真正解析出来的 IP，
// 所以把域名解析到 127.0.0.1 也过不去。
//
//	f := safefetch.New(safefetch.Options{MaxBytes: 4 << 20})
//	res, err := f.Fetch(ctx, userURL)
//	if err != nil { ... }
//	// res.Body 已经是 UTF-8 编码的响应内容；res.StatusCode 由调用方决定怎么处理
package safefetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

// ErrBlocked 表示地址指向本机、内网或链路本地等不该从公网服务访问的范围。
var ErrBlocked = errors.New("地址指向本机或内网，已拒绝访问")

// Options 控制抓取行为。零值表示使用默认值。
type Options struct {
	// Timeout 是整次请求（含连接和读取）的超时，默认 30 秒。
	Timeout time.Duration
	// MaxBytes 是原始响应体的最大字节数，默认 4 MiB。超出部分会被截断。
	MaxBytes int64
	// UserAgent 为空时使用一个通用的浏览器标识，减少被网站直接拒绝的概率。
	UserAgent string
	// HTTPSOnly 为真时只允许 https，跳转到 http 也会被拒绝。
	HTTPSOnly bool
}

// Result 是一次抓取的结果。
type Result struct {
	FinalURL    string // 跟随跳转之后的最终地址
	StatusCode  int
	ContentType string
	Body        []byte // 已转成 UTF-8 的内容，长度受 MaxBytes 约束
}

// Fetcher 可以复用，内部持有连接池，并发安全。
type Fetcher struct {
	client    *http.Client
	maxBytes  int64
	userAgent string
	httpsOnly bool
}

// New 创建抓取器。默认不允许访问本机与内网地址，也不走 HTTP 代理，
// 避免代理让 IP 检查只看到代理那一跳。
func New(opts Options) *Fetcher {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	ua := strings.TrimSpace(opts.UserAgent)
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
	}
	return &Fetcher{
		client:    newSafeClient(timeout, opts.HTTPSOnly),
		maxBytes:  maxBytes,
		userAgent: ua,
		httpsOnly: opts.HTTPSOnly,
	}
}

// Fetch 抓取一个用户填写的网址。非 2xx / 3xx 的状态码不会返回 error，
// 由调用方看 Result.StatusCode 决定怎么处理。
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	if f.httpsOnly && !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("只允许抓取 https 网址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("网址格式不正确")
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.6")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	body, err := DecodeBody(resp.Body, ct, f.maxBytes)
	if err != nil {
		return nil, err
	}
	finalURL := u.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return &Result{
		FinalURL:    finalURL,
		StatusCode:  resp.StatusCode,
		ContentType: ct,
		Body:        body,
	}, nil
}

// ValidateURL 把用户填的网址收拾成可用的 URL，并拒绝本机、内网的字面量地址。
// 没写协议时默认补 https://。真正的兜底在连接时检查解析出来的 IP。
func ValidateURL(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("请先填写网址")
	}
	if utf8.RuneCountInString(s) > 2000 {
		return nil, errors.New("网址太长了")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, errors.New("网址格式不正确")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("网址需要以 http:// 或 https:// 开头")
	}
	if u.User != nil {
		return nil, errors.New("网址里不要带账号密码")
	}
	if err := ValidateHost(u.Hostname()); err != nil {
		return nil, err
	}
	u.Fragment = ""
	return u, nil
}

// ValidateHost 拒绝 localhost、.local 和内网 IP 字面量。
// 域名最终解析到内网的情况由连接时的 IP 检查负责。
func ValidateHost(host string) error {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return errors.New("地址里没有域名")
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return ErrBlocked
	}
	if ip := net.ParseIP(h); ip != nil && blockedIP(ip) {
		return ErrBlocked
	}
	return nil
}

func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 运营商级 NAT（100.64.0.0/10）以及 0.0.0.0/8。
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		if v4[0] == 0 {
			return true
		}
	}
	return false
}

func newSafeClient(timeout time.Duration, httpsOnly bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return ErrBlocked
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// 不读环境变量里的代理：代理会让 IP 检查只看到代理的地址，等于放行内网。
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 6 {
				return errors.New("跳转次数太多")
			}
			if httpsOnly && !strings.EqualFold(req.URL.Scheme, "https") {
				return errors.New("只允许抓取 https 网址")
			}
			_, err := ValidateURL(req.URL.String())
			return err
		},
	}
}

// DecodeBody 读取响应体并把常见网页编码（GBK、GB2312、Big5、UTF-16 等）转成 UTF-8。
// maxBytes 限制原始响应的字节数；无法识别编码时按原样返回。
func DecodeBody(r io.Reader, contentType string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		raw = raw[:maxBytes]
	}
	if len(raw) == 0 {
		return raw, nil
	}
	decoded, err := charset.NewReader(bytes.NewReader(raw), contentType)
	if err != nil {
		return raw, nil
	}
	out, err := io.ReadAll(decoded)
	if err != nil {
		return nil, err
	}
	return out, nil
}
