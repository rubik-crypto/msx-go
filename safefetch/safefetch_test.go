package safefetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestValidateURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"example.com/a", "https://example.com/a"},
		{"https://example.com/a#frag", "https://example.com/a"},
		{"http://example.com:8080/x", "http://example.com:8080/x"},
	}
	for _, tc := range ok {
		u, err := ValidateURL(tc.in)
		if err != nil {
			t.Fatalf("%q 应该通过：%v", tc.in, err)
		}
		if u.String() != tc.want {
			t.Fatalf("%q 整理成 %q，想要 %q", tc.in, u.String(), tc.want)
		}
	}

	bad := []string{
		"", "http://127.0.0.1/", "http://localhost/", "https://10.0.0.1/",
		"https://192.168.1.1/", "https://[::1]/", "https://169.254.1.1/",
		"https://100.64.0.1/", "file:///etc/passwd", "https://user:pass@example.com/",
	}
	for _, raw := range bad {
		if _, err := ValidateURL(raw); err == nil {
			t.Fatalf("%q 应该被拒绝", raw)
		}
	}
}

func TestValidateHost(t *testing.T) {
	if err := ValidateHost("example.com"); err != nil {
		t.Fatalf("公网域名不该被拒绝：%v", err)
	}
	for _, host := range []string{"localhost", "foo.localhost", "printer.local", "127.0.0.1", "::1", "10.1.2.3"} {
		if err := ValidateHost(host); !errors.Is(err, ErrBlocked) {
			t.Fatalf("%q 应该返回 ErrBlocked，实际 %v", host, err)
		}
	}
}

func TestBlockedIP(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.0.1", "::1", "fc00::1", "0.0.0.0", "100.64.1.1"} {
		if !blockedIP(net.ParseIP(raw)) {
			t.Fatalf("%s 应该被拦截", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if blockedIP(net.ParseIP(raw)) {
			t.Fatalf("%s 不应该被拦截", raw)
		}
	}
}

func TestDecodeBodyGBK(t *testing.T) {
	raw := []byte(`<html><body>中文内容 88 元</body></html>`)
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeBody(bytes.NewReader(gbk), "text/html; charset=gbk", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "中文内容 88 元") {
		t.Fatalf("GBK 没有正确解码：%q", string(out))
	}
}

func TestDecodeBodyLimit(t *testing.T) {
	out, err := DecodeBody(strings.NewReader("0123456789abcdef"), "text/plain", 8)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "01234567" {
		t.Fatalf("应该只读 8 字节，实际 %q", string(out))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestFetchDecodesAndReportsStatus(t *testing.T) {
	raw := []byte(`<html><body>你好</body></html>`)
	gbk, _ := simplifiedchinese.GBK.NewEncoder().Bytes(raw)
	f := New(Options{MaxBytes: 1 << 20})
	f.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 404,
			Header:     http.Header{"Content-Type": []string{"text/html; charset=gbk"}},
			Body:       io.NopCloser(bytes.NewReader(gbk)),
			Request:    req,
		}, nil
	})}
	res, err := f.Fetch(context.Background(), "https://example.com/missing")
	if err != nil {
		t.Fatalf("非 2xx 不该由 Fetcher 报错：%v", err)
	}
	if res.StatusCode != 404 {
		t.Fatalf("状态码不对：%d", res.StatusCode)
	}
	if !strings.Contains(string(res.Body), "你好") {
		t.Fatalf("内容没有解码：%q", string(res.Body))
	}
	if res.FinalURL != "https://example.com/missing" {
		t.Fatalf("最终地址不对：%s", res.FinalURL)
	}
}

func TestFetchRejectsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not reach"))
	}))
	defer srv.Close()

	f := New(Options{})
	_, err := f.Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("本机地址应该返回 ErrBlocked，实际 %v", err)
	}
}

func TestFetchHTTPSOnly(t *testing.T) {
	f := New(Options{HTTPSOnly: true})
	if _, err := f.Fetch(context.Background(), "http://example.com/"); err == nil {
		t.Fatal("只允许 https 时应该拒绝 http")
	}
}
