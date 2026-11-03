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
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSubscription(t *testing.T) (Subscription, []byte, []byte) {
	t.Helper()
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成浏览器密钥失败: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("生成认证密钥失败: %v", err)
	}
	public := uaKey.PublicKey().Bytes()
	return Subscription{
		Endpoint: "https://fcm.googleapis.com/wp/test",
		P256DH:   base64.RawURLEncoding.EncodeToString(public),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}, public, auth
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.json")
	first, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("创建密钥失败: %v", err)
	}
	if first.PublicKey() == "" {
		t.Fatal("公钥为空")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("查看密钥文件失败: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("密钥文件权限 = %o", info.Mode().Perm())
	}
	second, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("读取密钥失败: %v", err)
	}
	if second.PublicKey() != first.PublicKey() {
		t.Fatal("重新加载后公钥变了")
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("写入坏文件失败: %v", err)
	}
	if _, err := LoadOrCreateKey(bad); err == nil {
		t.Fatal("损坏的密钥文件应当报错，而不是悄悄换一对新密钥")
	}
}

func TestEncryptPayloadRoundTrip(t *testing.T) {
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成浏览器密钥失败: %v", err)
	}
	uaPublic := uaKey.PublicKey().Bytes()
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("生成认证密钥失败: %v", err)
	}
	payload := []byte(`{"title":"提示","body":"有一条新消息"}`)
	body, err := EncryptPayload(payload, uaPublic, auth)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	salt := body[:16]
	if size := binary.BigEndian.Uint32(body[16:20]); size != uint32(recordSize) {
		t.Fatalf("记录长度 = %d", size)
	}
	if idLen := int(body[20]); idLen != 65 {
		t.Fatalf("临时公钥长度 = %d", idLen)
	}
	asPublic := body[21 : 21+65]
	ciphertext := body[21+65:]

	asKey, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		t.Fatalf("临时公钥无法解析: %v", err)
	}
	secret, err := uaKey.ECDH(asKey)
	if err != nil {
		t.Fatalf("协商密钥失败: %v", err)
	}
	info := append([]byte("WebPush: info\x00"), uaPublic...)
	info = append(info, asPublic...)
	prk, err := hkdf.Key(sha256.New, secret, auth, string(info), 32)
	if err != nil {
		t.Fatalf("派生密钥失败: %v", err)
	}
	cek, err := hkdf.Key(sha256.New, prk, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatalf("派生内容密钥失败: %v", err)
	}
	nonce, err := hkdf.Key(sha256.New, prk, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatalf("派生随机数失败: %v", err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatalf("初始化解密失败: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("初始化解密失败: %v", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if len(plaintext) != len(payload)+1 || plaintext[len(plaintext)-1] != 0x02 {
		t.Fatalf("明文长度或结束标记不对: %d", len(plaintext))
	}
	if !bytes.Equal(plaintext[:len(plaintext)-1], payload) {
		t.Fatalf("解密内容不一致: %s", plaintext[:len(plaintext)-1])
	}
}

func TestValidateEndpoint(t *testing.T) {
	allowed := []string{
		"https://fcm.googleapis.com/wp/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/def",
		"https://web.push.apple.com/QGVsbG8",
		"https://wns2-par02p.notify.windows.com/w/?token=abc",
	}
	for _, item := range allowed {
		if _, err := ValidateEndpoint(item); err != nil {
			t.Fatalf("应当允许 %s: %v", item, err)
		}
	}
	blocked := []string{
		"",
		"http://fcm.googleapis.com/wp/abc",
		"https://fcm.googleapis.com.evil.com/wp/abc",
		"https://127.0.0.1/push",
		"https://localhost:8443/push",
		"https://user:pass@fcm.googleapis.com/wp/abc",
		"https://fcm.googleapis.com:8443/wp/abc",
	}
	for _, item := range blocked {
		if _, err := ValidateEndpoint(item); !errors.Is(err, ErrEndpoint) {
			t.Fatalf("应当拒绝 %s: %v", item, err)
		}
	}
}

func TestAuthorization(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	client := NewClient(key, "mailto:ops@example.com")
	header, err := client.authorization("https://fcm.googleapis.com/wp/abc", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("生成签名失败: %v", err)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(header, "vapid t="), ", k="+key.PublicKey())
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 段数 = %d", len(parts))
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解析签名内容失败: %v", err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatalf("解析签名内容失败: %v", err)
	}
	if claims.Aud != "https://fcm.googleapis.com" || claims.Sub != "mailto:ops@example.com" || claims.Exp <= 1700000000 {
		t.Fatalf("签名内容不对: %+v", claims)
	}

	publicRaw, err := base64.RawURLEncoding.DecodeString(key.PublicKey())
	if err != nil {
		t.Fatalf("解析公钥失败: %v", err)
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), publicRaw)
	if x == nil {
		t.Fatal("公钥不是未压缩格式")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("解析签名失败: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, digest[:],
		new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("签名验证失败")
	}

	if _, err := client.authorization("https://fcm.googleapis.com/wp/abc", time.Now()); err != nil {
		t.Fatalf("生成签名失败: %v", err)
	}
	badSubject := NewClient(key, "someone@example.com")
	if _, err := badSubject.authorization("https://fcm.googleapis.com/wp/abc", time.Now()); err == nil {
		t.Fatal("不合规的联系方式应当报错")
	}
}

func TestSend(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	var gotAuth string
	var gotEncoding string
	var gotBody []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	sub, _, _ := testSubscription(t)
	sub.Endpoint = server.URL + "/push"
	client := NewClient(key, "mailto:ops@example.com")
	client.HTTP = server.Client()
	client.validateEndpoint = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	if err := client.Send(context.Background(), sub, []byte(`{"title":"提示"}`)); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "vapid t=") || gotEncoding != "aes128gcm" || len(gotBody) < 100 {
		t.Fatalf("请求内容不完整: auth=%q encoding=%q body=%d", gotAuth, gotEncoding, len(gotBody))
	}

	gone := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer gone.Close()
	sub.Endpoint = gone.URL + "/push"
	client.HTTP = gone.Client()
	err = client.Send(context.Background(), sub, []byte("hello"))
	if !errors.Is(err, ErrGone) {
		t.Fatalf("410 应当返回 ErrGone，实际 = %v", err)
	}

	client.validateEndpoint = nil
	if err := client.Send(context.Background(), Subscription{Endpoint: "http://127.0.0.1/push"}, nil); !errors.Is(err, ErrEndpoint) {
		t.Fatalf("不可信地址应当返回 ErrEndpoint，实际 = %v", err)
	}
	if err := client.Send(context.Background(), Subscription{
		Endpoint: "https://fcm.googleapis.com/wp/abc",
		P256DH:   "bad",
		Auth:     "bad",
	}, nil); !errors.Is(err, ErrSubscription) {
		t.Fatalf("错误密钥应当返回 ErrSubscription，实际 = %v", err)
	}
}
