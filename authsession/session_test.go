package authsession

import (
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testManager(t *testing.T, now *time.Time) *Manager {
	t.Helper()
	secret, err := NewSecret(32)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := New(secret, Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr.Now = func() time.Time { return *now }
	return mgr
}

func TestIssueAndVerify(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mgr := testManager(t, &now)

	token, err := mgr.Issue("owner", 3)
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := mgr.Verify(token, "owner", 3, now)
	if !ok {
		t.Fatal("正常令牌应该通过")
	}
	if claims.Username != "owner" || claims.Version != 3 || claims.Nonce == "" {
		t.Fatalf("令牌内容不对: %+v", claims)
	}
	if claims.Expired(now) {
		t.Fatal("刚签发的令牌不应该过期")
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mgr := testManager(t, &now)
	token, err := mgr.Issue("owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	tampered := token[:len(token)-1] + "0"
	if tampered == token {
		tampered = token[:len(token)-1] + "1"
	}

	cases := []struct {
		name  string
		token string
		user  string
		ver   int64
		at    time.Time
	}{
		{"空令牌", "", "owner", 1, now},
		{"签名被改", tampered, "owner", 1, now},
		{"账号不符", token, "other", 1, now},
		{"版本不符", token, "owner", 2, now},
		{"已经过期", token, "owner", 1, now.Add(DefaultTTL + time.Minute)},
		{"没有分隔点", strings.ReplaceAll(token, ".", ""), "owner", 1, now},
	}
	for _, c := range cases {
		if _, ok := mgr.Verify(c.token, c.user, c.ver, c.at); ok {
			t.Fatalf("%s: 不应该通过", c.name)
		}
	}

	// 另一把密钥签发的令牌也不行。
	other, err := New([]byte(strings.Repeat("k", 32)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	other.Now = func() time.Time { return now }
	otherToken, err := other.Issue("owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Verify(otherToken, "owner", 1, now); ok {
		t.Fatal("别的密钥签发的令牌不应该通过")
	}
}

func TestVerifyRejectsAbsurdExpiry(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mgr := testManager(t, &now)
	token, err := mgr.Issue("owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	// 把有效期往后拉一年再用同一把密钥签名，模拟被改过的令牌。
	payload := `{"u":"owner","e":` + strconv.FormatInt(now.Add(365*24*time.Hour).Unix(), 10) + `,"v":1,"n":"x"}`
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	forged := encoded + "." + mgr.sign(encoded)
	if strings.Split(token, ".")[0] == encoded {
		t.Fatal("测试构造的令牌不应该和正常令牌一样")
	}
	if _, ok := mgr.Verify(forged, "owner", 1, now); ok {
		t.Fatal("有效期明显超长的令牌应该被拒绝")
	}
}

func TestManagerRejectsShortSecret(t *testing.T) {
	if _, err := New([]byte("short"), Options{}); err == nil {
		t.Fatal("太短的密钥应该被拒绝")
	}
}

func TestSecretEncodeDecode(t *testing.T) {
	secret, err := NewSecret(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 32 {
		t.Fatalf("默认密钥长度应为 32，实际 %d", len(secret))
	}
	decoded, err := DecodeSecret(EncodeSecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(secret) {
		t.Fatal("编码再解码后密钥不一致")
	}
	if _, err := DecodeSecret("abcd"); err == nil {
		t.Fatal("太短的密钥串应该被拒绝")
	}
	if _, err := DecodeSecret("zzzz"); err == nil {
		t.Fatal("非十六进制密钥串应该被拒绝")
	}
}

func TestCookieAttributes(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mgr := testManager(t, &now)
	token, err := mgr.Issue("owner", 1)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://example.com/login", nil)
	rec := httptest.NewRecorder()
	mgr.SetCookie(rec, req, token)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应该写一个 cookie，实际 %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != DefaultCookieName || cookie.Value != token {
		t.Fatalf("cookie 名字或内容不对: %+v", cookie)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie 必须 HttpOnly 且 SameSite=Strict: %+v", cookie)
	}
	if cookie.Secure {
		t.Fatal("普通 HTTP 请求不应该要求 Secure")
	}
	if cookie.Path != "/" || cookie.MaxAge != int(DefaultTTL/time.Second) {
		t.Fatalf("cookie 路径或有效期不对: %+v", cookie)
	}

	// 反向代理声明 HTTPS 时要带上 Secure。
	proxyReq := httptest.NewRequest(http.MethodPost, "http://example.com/login", nil)
	proxyReq.Header.Set("X-Forwarded-Proto", "https")
	proxyRec := httptest.NewRecorder()
	mgr.SetCookie(proxyRec, proxyReq, token)
	if got := proxyRec.Result().Cookies()[0]; !got.Secure {
		t.Fatal("代理转发 HTTPS 时 cookie 应该带 Secure")
	}

	// 直接走 TLS 时也要带 Secure。
	tlsReq := httptest.NewRequest(http.MethodPost, "https://example.com/login", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	tlsRec := httptest.NewRecorder()
	mgr.SetCookie(tlsRec, tlsReq, token)
	if got := tlsRec.Result().Cookies()[0]; !got.Secure {
		t.Fatal("TLS 请求的 cookie 应该带 Secure")
	}

	// 退出时让浏览器删除 cookie。
	clearRec := httptest.NewRecorder()
	mgr.ClearCookie(clearRec, req)
	if got := clearRec.Result().Cookies()[0]; got.MaxAge != -1 || got.Value != "" {
		t.Fatalf("清除 cookie 的参数不对: %+v", got)
	}
}

func TestSessionReadsCookie(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mgr := testManager(t, &now)
	token, err := mgr.Issue("owner", 4)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/state", nil)
	req.AddCookie(&http.Cookie{Name: mgr.CookieName(), Value: token})

	claims, ok := mgr.Session(req, "owner", 4)
	if !ok || claims.Username != "owner" {
		t.Fatal("从 cookie 读出的登录状态应该有效")
	}
	if _, ok := mgr.Session(req, "owner", 5); ok {
		t.Fatal("凭据版本不符时不应该通过")
	}

	empty := httptest.NewRequest(http.MethodGet, "http://example.com/api/state", nil)
	if _, ok := mgr.Session(empty, "owner", 4); ok {
		t.Fatal("没有 cookie 时不应该通过")
	}
}
