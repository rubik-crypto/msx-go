package authsession

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultCookieName 是 cookie 的默认名字。同一个域名下挂多个项目时，
	// 每个项目应该换一个不同的名字，避免互相覆盖。
	DefaultCookieName = "admin_session"
	// DefaultTTL 是登录状态的默认有效期。
	DefaultTTL = 7 * 24 * time.Hour
	// MinSecretLength 是签名密钥的最短长度。
	MinSecretLength = 16
	// 正常令牌只有一两百字节，过长的直接拒绝，避免无谓的解码和内存占用。
	maxTokenLength = 2048
)

// Claims 是 cookie 里携带的登录信息，不包含密码或权限内容。
type Claims struct {
	Username  string `json:"u"`
	ExpiresAt int64  `json:"e"`
	Version   int64  `json:"v"`
	Nonce     string `json:"n"`
}

// Expired 报告令牌在 now 时刻是否已经过期。
func (c Claims) Expired(now time.Time) bool {
	return c.ExpiresAt == 0 || now.Unix() >= c.ExpiresAt
}

// Options 是 Manager 的可选配置，零值会自动补成默认值。
type Options struct {
	// CookieName 为空时使用 DefaultCookieName。
	CookieName string
	// CookiePath 为空时使用 "/"。站点挂在子路径下时也可以保持 "/"。
	CookiePath string
	// TTL 小于等于 0 时使用 DefaultTTL。
	TTL time.Duration
}

// Manager 负责签发、校验和写入登录 cookie。它可以被多个请求并发使用。
type Manager struct {
	cookieName string
	cookiePath string
	ttl        time.Duration
	secret     []byte

	// Now 可选，用来替换当前时间，方便测试。生产环境留空即可。
	Now func() time.Time
}

// New 用签名密钥创建 Manager。密钥至少要有 MinSecretLength 字节，
// 并且要持久化保存；换密钥会让所有已经签发的 cookie 失效。
func New(secret []byte, opts Options) (*Manager, error) {
	if len(secret) < MinSecretLength {
		return nil, errors.New("authsession: 签名密钥太短")
	}
	name := strings.TrimSpace(opts.CookieName)
	if name == "" {
		name = DefaultCookieName
	}
	path := strings.TrimSpace(opts.CookiePath)
	if path == "" {
		path = "/"
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Manager{
		cookieName: name,
		cookiePath: path,
		ttl:        ttl,
		secret:     append([]byte(nil), secret...),
	}, nil
}

// NewSecret 生成一个随机的签名密钥，size 小于 MinSecretLength 时按 32 字节生成。
func NewSecret(size int) ([]byte, error) {
	if size < MinSecretLength {
		size = 32
	}
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return nil, errors.New("authsession: 生成签名密钥失败")
	}
	return buf, nil
}

// EncodeSecret 把密钥编码成可以写进配置文件的十六进制字符串。
func EncodeSecret(secret []byte) string {
	return hex.EncodeToString(secret)
}

// DecodeSecret 解开 EncodeSecret 的结果，并校验长度。
func DecodeSecret(encoded string) ([]byte, error) {
	secret, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(secret) < MinSecretLength {
		return nil, errors.New("authsession: 签名密钥格式不正确")
	}
	return secret, nil
}

// CookieName 返回当前使用的 cookie 名字。
func (m *Manager) CookieName() string { return m.cookieName }

// TTL 返回登录状态的有效期。
func (m *Manager) TTL() time.Duration { return m.ttl }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Issue 为某个账号签发登录令牌。version 是调用方的凭据版本，
// 修改密码后把它加一，旧令牌在校验时就会因为版本不一致而失效。
func (m *Manager) Issue(username string, version int64) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", errors.New("authsession: 账号不能为空")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("authsession: 生成随机数失败")
	}
	claims := Claims{
		Username:  username,
		ExpiresAt: m.now().Add(m.ttl).Unix(),
		Version:   version,
		Nonce:     hex.EncodeToString(nonce),
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", errors.New("authsession: 生成令牌失败")
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + m.sign(payload), nil
}

// Verify 校验令牌的签名、有效期、账号和凭据版本。
func (m *Manager) Verify(token, username string, version int64, now time.Time) (Claims, bool) {
	var claims Claims
	if len(token) == 0 || len(token) > maxTokenLength {
		return claims, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return claims, false
	}
	expected := m.sign(parts[0])
	if len(expected) != len(parts[1]) ||
		subtle.ConstantTimeCompare([]byte(expected), []byte(parts[1])) != 1 {
		return claims, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, false
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return claims, false
	}
	if claims.Username == "" || claims.Expired(now) {
		return claims, false
	}
	// 有效期明显超出正常范围，说明令牌被改过或用错了密钥。
	if claims.ExpiresAt > now.Add(m.ttl+time.Hour).Unix() {
		return claims, false
	}
	if claims.Version != version {
		return claims, false
	}
	if subtle.ConstantTimeCompare([]byte(claims.Username), []byte(username)) != 1 {
		return claims, false
	}
	return claims, true
}

// SetCookie 把令牌写进 HttpOnly cookie。Secure 会按请求是否走 HTTPS 自动决定，
// 因此放在反向代理后面时要让代理带上 X-Forwarded-Proto。
func (m *Manager) SetCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    token,
		Path:     m.cookiePath,
		MaxAge:   int(m.ttl / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	})
}

// ClearCookie 让浏览器删掉登录 cookie。它只影响当前浏览器，
// 要让所有设备下线需要提升调用方的凭据版本。
func (m *Manager) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    "",
		Path:     m.cookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	})
}

// Token 从请求里取出登录令牌。
func (m *Manager) Token(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(m.cookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

// Session 从请求里取出并校验登录状态，是 Token + Verify 的组合。
func (m *Manager) Session(r *http.Request, username string, version int64) (Claims, bool) {
	token, ok := m.Token(r)
	if !ok {
		return Claims{}, false
	}
	return m.Verify(token, username, version, m.now())
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
