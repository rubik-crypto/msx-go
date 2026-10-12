package realip

import (
	"net/http/httptest"
	"testing"
)

func TestFrom(t *testing.T) {
	cases := []struct {
		name string
		xff  string
		xr   string
		ra   string
		want string
	}{
		{"没有代理头就用连接对端", "", "", "9.9.9.9:1234", "9.9.9.9"},
		{"单个 XFF（一个反代追加的一般长相）", "8.8.8.8", "", "10.0.0.1:1234", "8.8.8.8"},
		{"客户端伪造的一段在最左边，被跳过", "1.2.3.4(spoofed), 8.8.8.8", "", "10.0.0.1:5", "8.8.8.8"},
		{"多跳且内网在右边", "8.8.8.8, 10.1.2.3", "", "10.0.0.1:5", "8.8.8.8"},
		{"整条链都是内网时退回最右一跳", "10.1.1.1, 10.2.2.2", "", "10.0.0.1:5", "10.2.2.2"},
		{"没有 XFF 就用 X-Real-Ip", "", "7.7.7.7", "10.0.0.1:5", "7.7.7.7"},
		{"全是空的返回空串", "", "", "", ""},
		{"IPv6 也认", "2606:4700::1111, 10.0.0.1", "", "[::1]:5", "2606:4700::1111"},
		{"文档示例段不算公网", "192.0.2.5, 8.8.4.4", "", "10.0.0.1:5", "8.8.4.4"},
	}
	for _, c := range cases {
		if got := From(c.xff, c.xr, c.ra); got != c.want {
			t.Errorf("%s：From() = %q，应该是 %q", c.name, got, c.want)
		}
	}
}

// 最要紧的一条：客户端**在前面多加几段**伪造地址时，不能取到它。
func TestSpoofedFirstEntryIgnored(t *testing.T) {
	// 反代把真实客户端追加在右边（$proxy_add_x_forwarded_for 的写法）：
	// 客户端自带的 1.2.3.4 留在最左边，必须跳过它。
	got := From("1.2.3.4, 93.184.216.34", "", "10.0.0.1:80")
	if got != "93.184.216.34" {
		t.Fatalf("应该取最右边的公网地址，得到 %q", got)
	}
	// 中间还有一跳内网，也要跳过。
	got2 := From("1.2.3.4, 10.5.5.5, 93.184.216.34", "", "10.0.0.1:80")
	if got2 != "93.184.216.34" {
		t.Fatalf("应该取最右边的公网地址，得到 %q", got2)
	}
	// 只有一段时无法分辨「反代追加的」和「客户端自己写的」，
	// 按约定当成真实地址（见包注释里那条限制）。
	if got3 := From("1.2.3.4", "", "10.0.0.1:80"); got3 != "1.2.3.4" {
		t.Fatalf("单段 XFF 按约定当成真实地址，得到 %q", got3)
	}
}

func TestFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 93.184.216.34")
	if got := FromRequest(r); got != "93.184.216.34" {
		t.Fatalf("FromRequest() = %q", got)
	}
	if FromRequest(nil) != "" {
		t.Fatalf("nil 请求应该返回空串")
	}
}

func TestHost(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:80":  "1.2.3.4",
		"[::1]:80":    "::1",
		"1.2.3.4":     "1.2.3.4",
		"  ":          "",
		"":            "",
	}
	for in, want := range cases {
		if got := Host(in); got != want {
			t.Errorf("Host(%q) = %q，应该是 %q", in, got, want)
		}
	}
}

func TestIsPublic(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700::1111"}
	private := []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "127.0.0.1",
		"169.254.1.1", "100.64.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1",
		"198.18.0.1", "0.0.0.0", "224.0.0.1", "::1", "fe80::1",
		"", "example.com", "不是地址"}
	for _, s := range public {
		if !IsPublic(s) {
			t.Errorf("%q 应该算公网地址", s)
		}
	}
	for _, s := range private {
		if IsPublic(s) {
			t.Errorf("%q 不该算公网地址", s)
		}
	}
}
