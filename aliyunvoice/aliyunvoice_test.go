package aliyunvoice

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		AccessKeyID:      "LTAI5tTestKey123",
		AccessKeySecret:  "TestSecretABC1234567890",
		TtsCode:          "TTS_123456789",
		CalledNumber:     "13800000000",
		CalledShowNumber: "057188888888",
	}
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("合法配置被拒绝: %v", err)
	}
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"缺少密钥", func(c *Config) { c.AccessKeySecret = "" }},
		{"缺少模板", func(c *Config) { c.TtsCode = "" }},
		{"缺少号码", func(c *Config) { c.CalledNumber = "" }},
		{"密钥带空格", func(c *Config) { c.AccessKeySecret = "secret with space" }},
		{"模板编号非法", func(c *Config) { c.TtsCode = "TTS 123" }},
		{"号码非法", func(c *Config) { c.CalledNumber = "abc" }},
		{"外呼号码非法", func(c *Config) { c.CalledShowNumber = "***" }},
		{"变量名非法", func(c *Config) { c.TemplateParams = map[string]string{"bad key": "v"} }},
	}
	for _, item := range cases {
		cfg := testConfig()
		item.change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s 没有被拒绝", item.name)
		}
	}
}

// 期望签名由独立的 Python 脚本按阿里云 RPC 签名规则算出。
func TestBuildRequestSignature(t *testing.T) {
	client := New(nil)
	req, err := client.buildRequest(
		context.Background(),
		testConfig(),
		time.Date(2026, 11, 3, 8, 30, 0, 0, time.UTC),
		"fixednonce1234567890abcdef",
	)
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("读取请求体失败: %v", err)
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatalf("请求体不是表单: %v", err)
	}
	if got := form.Get("Signature"); got != "ppRWFL0NAUR2L5kaHhDZZGO/8mk=" {
		t.Fatalf("签名 = %q", got)
	}
	if form.Get("Action") != "SingleCallByTts" || form.Get("Version") != "2017-05-25" {
		t.Fatalf("公共参数不对: %s", body)
	}
	if form.Get("CalledNumber") != "13800000000" || form.Get("TtsCode") != "TTS_123456789" {
		t.Fatalf("业务参数不对: %s", body)
	}
	if strings.Contains(string(body), testConfig().AccessKeySecret) {
		t.Fatal("请求体里出现了 AccessKey Secret")
	}
}

func TestBuildRequestTemplateParams(t *testing.T) {
	cfg := testConfig()
	cfg.TemplateParams = map[string]string{"amount": "100"}
	client := New(nil)
	req, err := client.buildRequest(context.Background(), cfg, time.Now(), "nonce")
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	if form.Get("TtsParam") != `{"amount":"100"}` {
		t.Fatalf("模板变量 = %q", form.Get("TtsParam"))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func fakeClient(status int, body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
}

func TestCallSuccess(t *testing.T) {
	client := New(fakeClient(http.StatusOK, `{"Code":"OK","Message":"OK","RequestId":"r1","CallId":"c1"}`))
	result, err := client.Call(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("拨打失败: %v", err)
	}
	if result.CallID != "c1" || result.RequestID != "r1" {
		t.Fatalf("回执 = %+v", result)
	}
}

func TestCallRejected(t *testing.T) {
	client := New(fakeClient(http.StatusOK, `{"Code":"isv.BUSINESS_LIMIT_CONTROL","Message":"呼叫超限"}`))
	_, err := client.Call(context.Background(), testConfig())
	if err == nil || !strings.Contains(err.Error(), "呼叫超限") {
		t.Fatalf("拒绝原因没有返回: %v", err)
	}
	if strings.Contains(err.Error(), testConfig().AccessKeySecret) {
		t.Fatal("错误信息泄漏了 AccessKey Secret")
	}
}

func TestCallInvalidConfigSkipsHTTP(t *testing.T) {
	called := false
	client := New(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})})
	cfg := testConfig()
	cfg.CalledNumber = ""
	if _, err := client.Call(context.Background(), cfg); err == nil {
		t.Fatal("不完整配置没有报错")
	}
	if called {
		t.Fatal("配置不完整时不应发出网络请求")
	}
}
