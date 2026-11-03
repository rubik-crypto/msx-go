package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	recordSize   = 4096
	defaultTTL   = 3600
	defaultAgent = "webpush/1.0"
)

var (
	// ErrGone 表示推送服务明确告知订阅已经失效（HTTP 404 或 410），
	// 调用方应当把这台设备的订阅记录删掉。
	ErrGone = errors.New("webpush: 订阅已失效")
	// ErrEndpoint 表示推送地址不是受信任的浏览器推送服务。
	ErrEndpoint = errors.New("webpush: 推送地址不被信任")
	// ErrSubscription 表示订阅里的公钥或认证密钥格式不正确。
	ErrSubscription = errors.New("webpush: 订阅密钥格式不正确")
)

// 只允许浏览器厂商的推送服务，防止订阅地址被替换成内网地址。
var allowedHosts = []string{
	"fcm.googleapis.com",
	"android.googleapis.com",
	"updates.push.services.mozilla.com",
	"push.services.mozilla.com",
	"web.push.apple.com",
	".notify.windows.com",
	".push.microsoft.com",
}

// Subscription 是浏览器通过 PushManager.subscribe 得到的订阅信息。
// Endpoint、P256DH 和 Auth 都按浏览器给出的原样保存即可。
type Subscription struct {
	Endpoint string
	P256DH   string
	Auth     string
}

// Key 是一对 VAPID 密钥。公钥交给浏览器订阅时使用，私钥只留在服务端。
type Key struct {
	private *ecdsa.PrivateKey
	public  string
}

type keyFile struct {
	PrivateKey string    `json:"private_key"`
	PublicKey  string    `json:"public_key,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// NewKey 生成一对新的 VAPID 密钥（P-256）。
func NewKey() (*Key, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: 生成密钥失败: %w", err)
	}
	return newKey(private)
}

func newKey(private *ecdsa.PrivateKey) (*Key, error) {
	public, err := private.PublicKey.ECDH()
	if err != nil {
		return nil, fmt.Errorf("webpush: 导出公钥失败: %w", err)
	}
	return &Key{
		private: private,
		public:  base64.RawURLEncoding.EncodeToString(public.Bytes()),
	}, nil
}

// PublicKey 返回 base64url 编码的公钥，可直接交给前端的 applicationServerKey。
func (k *Key) PublicKey() string {
	return k.public
}

// LoadOrCreateKey 从 path 读取 VAPID 密钥；文件不存在时生成一对并原子写入。
// 文件损坏或无法解析时返回错误，不会悄悄换掉密钥导致已订阅的设备全部失效。
func LoadOrCreateKey(path string) (*Key, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return parseKeyFile(raw)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("webpush: 读取密钥失败: %w", err)
	}
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	if err := saveKey(path, key); err != nil {
		return nil, err
	}
	return key, nil
}

func parseKeyFile(raw []byte) (*Key, error) {
	var file keyFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("webpush: 密钥文件格式错误: %w", err)
	}
	der, err := decodeAnyBase64(file.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("webpush: 密钥文件内容错误: %w", err)
	}
	private, err := x509.ParseECPrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("webpush: 私钥无法解析: %w", err)
	}
	if private.Curve != elliptic.P256() {
		return nil, errors.New("webpush: 密钥不是 P-256")
	}
	return newKey(private)
}

func saveKey(path string, key *Key) error {
	der, err := x509.MarshalECPrivateKey(key.private)
	if err != nil {
		return fmt.Errorf("webpush: 整理私钥失败: %w", err)
	}
	raw, err := json.MarshalIndent(keyFile{
		PrivateKey: base64.RawStdEncoding.EncodeToString(der),
		PublicKey:  key.public,
		CreatedAt:  time.Now(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("webpush: 整理密钥文件失败: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("webpush: 创建数据目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".vapid-*.tmp")
	if err != nil {
		return fmt.Errorf("webpush: 创建临时文件失败: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("webpush: 设置密钥权限失败: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("webpush: 写入密钥失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("webpush: 同步密钥失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("webpush: 关闭密钥文件失败: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("webpush: 保存密钥失败: %w", err)
	}
	return nil
}

// Client 负责签名和发送推送。一个进程创建一个 Client 就够，可以并发调用 Send。
type Client struct {
	Key     *Key
	Subject string
	TTL     int
	Urgency string
	HTTP    *http.Client

	now              func() time.Time
	validateEndpoint func(string) (*url.URL, error)
}

// NewClient 创建推送客户端。subject 是 VAPID 规定的联系方式，
// 必须是以 mailto: 或 https:// 开头的地址，推送服务可能会用它联系你。
func NewClient(key *Key, subject string) *Client {
	return &Client{
		Key:     key,
		Subject: strings.TrimSpace(subject),
		TTL:     defaultTTL,
		Urgency: "high",
		now:     time.Now,
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          4,
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       45 * time.Second,
				TLSHandshakeTimeout:   8 * time.Second,
				ResponseHeaderTimeout: 12 * time.Second,
			},
		},
	}
}

// ValidateEndpoint 检查推送地址是否是受信任的浏览器推送服务。
func ValidateEndpoint(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return nil, ErrEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, ErrEndpoint
	}
	if u.Port() != "" && u.Port() != "443" {
		return nil, ErrEndpoint
	}
	host := strings.ToLower(u.Hostname())
	for _, item := range allowedHosts {
		if strings.HasPrefix(item, ".") {
			if strings.HasSuffix(host, item) {
				return u, nil
			}
			continue
		}
		if host == item {
			return u, nil
		}
	}
	return nil, ErrEndpoint
}

func decodeAnyBase64(raw string) ([]byte, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, ErrSubscription
	}
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, ErrSubscription
}

func decodeKey(raw string, wantLen int) ([]byte, error) {
	decoded, err := decodeAnyBase64(raw)
	if err != nil || len(decoded) != wantLen {
		return nil, ErrSubscription
	}
	return decoded, nil
}

// EncryptPayload 按 RFC 8291 的 aes128gcm 单条记录格式加密内容。
// 返回的字节直接作为推送请求体。
func EncryptPayload(payload, uaPublic, authSecret []byte) ([]byte, error) {
	curve := ecdh.P256()
	uaKey, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: 浏览器公钥无法使用: %w", err)
	}
	ephemeral, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: 生成临时密钥失败: %w", err)
	}
	asPublic := ephemeral.PublicKey().Bytes()
	secret, err := ephemeral.ECDH(uaKey)
	if err != nil {
		return nil, fmt.Errorf("webpush: 协商密钥失败: %w", err)
	}

	info := append([]byte("WebPush: info\x00"), uaPublic...)
	info = append(info, asPublic...)
	prk, err := hkdf.Key(sha256.New, secret, authSecret, string(info), 32)
	if err != nil {
		return nil, fmt.Errorf("webpush: 派生密钥失败: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("webpush: 生成随机盐失败: %w", err)
	}
	cek, err := hkdf.Key(sha256.New, prk, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, fmt.Errorf("webpush: 派生内容密钥失败: %w", err)
	}
	nonce, err := hkdf.Key(sha256.New, prk, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, fmt.Errorf("webpush: 派生随机数失败: %w", err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("webpush: 初始化加密失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webpush: 初始化加密失败: %w", err)
	}
	plaintext := append(append([]byte(nil), payload...), 0x02)
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	header := make([]byte, 0, 16+4+1+len(asPublic))
	header = append(header, salt...)
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, recordSize)
	header = append(header, size...)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	return append(header, ciphertext...), nil
}

func (c *Client) authorization(endpoint string, now time.Time) (string, error) {
	if c.Key == nil {
		return "", errors.New("webpush: 缺少 VAPID 密钥")
	}
	subject := strings.TrimSpace(c.Subject)
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https://") {
		return "", errors.New("webpush: 联系方式必须是以 mailto: 或 https:// 开头的地址")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", ErrEndpoint
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(12 * time.Hour).Unix(),
		"sub": subject,
	})
	if err != nil {
		return "", fmt.Errorf("webpush: 整理签名内容失败: %w", err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, c.Key.private, digest[:])
	if err != nil {
		return "", fmt.Errorf("webpush: 签名失败: %w", err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return "vapid t=" + signingInput + "." + base64.RawURLEncoding.EncodeToString(signature) +
		", k=" + c.Key.PublicKey(), nil
}

// Send 向一台设备推送一条内容。返回的错误可以用 errors.Is 判断：
// ErrGone 表示订阅已失效，应当删除；ErrEndpoint 表示地址不可信；ErrSubscription
// 表示浏览器给的密钥格式不对。
func (c *Client) Send(ctx context.Context, sub Subscription, payload []byte) error {
	check := c.validateEndpoint
	if check == nil {
		check = ValidateEndpoint
	}
	endpoint, err := check(sub.Endpoint)
	if err != nil {
		return err
	}
	uaPublic, err := decodeKey(sub.P256DH, 65)
	if err != nil || uaPublic[0] != 0x04 {
		return ErrSubscription
	}
	authSecret, err := decodeKey(sub.Auth, 16)
	if err != nil {
		return ErrSubscription
	}
	body, err := EncryptPayload(payload, uaPublic, authSecret)
	if err != nil {
		return err
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	authorization, err := c.authorization(endpoint.String(), now())
	if err != nil {
		return err
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("webpush: 创建请求失败")
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	urgency := strings.TrimSpace(c.Urgency)
	if urgency == "" {
		urgency = "high"
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.Header.Set("TTL", strconv.Itoa(ttl))
	req.Header.Set("Urgency", urgency)
	req.Header.Set("Authorization", authorization)

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("webpush: 推送超时: %w", ctx.Err())
		}
		return fmt.Errorf("webpush: 连接推送服务失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w（HTTP %d）", ErrGone, resp.StatusCode)
	default:
		return fmt.Errorf("webpush: 推送服务返回 HTTP %d", resp.StatusCode)
	}
}
