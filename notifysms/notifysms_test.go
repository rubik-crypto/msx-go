package notifysms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPercentEncode(t *testing.T) {
	cases := map[string]string{
		"a b":      "a%20b",
		"a+b":      "a%2Bb",
		"A-z_0.9~": "A-z_0.9~",
		"中文":       "%E4%B8%AD%E6%96%87",
		"*":        "%2A",
	}
	for input, want := range cases {
		if got := percentEncode(input); got != want {
			t.Fatalf("%q 应该编码成 %q，实际 %q", input, want, got)
		}
	}
}

func TestCanonicalQuerySortsKeys(t *testing.T) {
	got := canonicalQuery(map[string]string{"b": "2", "a": "1 2", "c": "+"})
	want := "a=1%202&b=2&c=%2B"
	if got != want {
		t.Fatalf("参数拼装不对：%q，期望 %q", got, want)
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"13800138000", "13800138000", true},
		{"+86 138-0013-8000", "+8613800138000", true},
		{"(010) 8888 6666", "01088886666", true},
		{"abc", "", false},
		{"123", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, err := NormalizePhone(tc.input)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Fatalf("%q 应该整理成 %q，实际 %q err=%v", tc.input, tc.want, got, err)
			}
		} else if err == nil {
			t.Fatalf("%q 应该被拒绝，实际得到 %q", tc.input, got)
		}
	}
}

func TestSendAliyunSignsRequest(t *testing.T) {
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"Code":"OK","Message":"OK"}`)
	}))
	defer server.Close()

	cfg := AliyunConfig{
		AccessKeyID:     "test-key",
		AccessKeySecret: "test-secret",
		SignName:        "测试签名",
		TemplateCode:    "SMS_123456",
		Endpoint:        server.URL,
	}
	if err := SendAliyun(context.Background(), cfg, "+86 138-0013-8000", "任务失败：余额不足"); err != nil {
		t.Fatalf("发送应该成功: %v", err)
	}
	if got.Get("Signature") == "" {
		t.Fatal("请求里应该带签名")
	}
	if got.Get("Action") != "SendSms" || got.Get("Version") != "2017-05-25" {
		t.Fatalf("接口参数不对: %v", got)
	}
	if got.Get("PhoneNumbers") != "+8613800138000" {
		t.Fatalf("手机号应该先整理再发送: %q", got.Get("PhoneNumbers"))
	}
	if got.Get("SignName") != "测试签名" || got.Get("TemplateCode") != "SMS_123456" {
		t.Fatalf("签名或模板编号不对: %v", got)
	}
	var param map[string]string
	if err := json.Unmarshal([]byte(got.Get("TemplateParam")), &param); err != nil {
		t.Fatalf("模板参数应该是 JSON: %v", err)
	}
	if param["content"] != "任务失败：余额不足" {
		t.Fatalf("模板参数里的正文不对: %v", param)
	}
	if got.Get("Timestamp") == "" || !strings.HasSuffix(got.Get("Timestamp"), "Z") {
		t.Fatalf("时间戳应该是 UTC 格式: %q", got.Get("Timestamp"))
	}
}

func TestSendAliyunBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Code":"isv.BUSINESS_LIMIT_CONTROL","Message":"触发小时级流控"}`)
	}))
	defer server.Close()
	cfg := AliyunConfig{
		AccessKeyID:     "test-key",
		AccessKeySecret: "test-secret",
		SignName:        "测试签名",
		TemplateCode:    "SMS_123456",
		Endpoint:        server.URL,
	}
	err := SendAliyun(context.Background(), cfg, "13800138000", "测试")
	if err == nil || !strings.Contains(err.Error(), "触发小时级流控") {
		t.Fatalf("业务错误应该带出原因: %v", err)
	}
}

func TestSendAliyunRejectsIncompleteConfig(t *testing.T) {
	err := SendAliyun(context.Background(), AliyunConfig{}, "13800138000", "测试")
	if err == nil || !strings.Contains(err.Error(), "AccessKey") {
		t.Fatalf("缺少配置时应该明确提示: %v", err)
	}
}

func TestSendGatewayReplacesPlaceholders(t *testing.T) {
	var gotPhone, gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPhone = r.URL.Query().Get("phone")
		gotText = r.URL.Query().Get("text")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := SendGateway(context.Background(), GatewayConfig{
		URLTemplate:  server.URL + "/send?phone={phone}&text={text}",
		AllowPrivate: true,
	}, "13800138000", "余额不够付手续费")
	if err != nil {
		t.Fatalf("发送应该成功: %v", err)
	}
	if gotPhone != "13800138000" || !strings.Contains(gotText, "余额不够") {
		t.Fatalf("网关收到的内容不对: phone=%q text=%q", gotPhone, gotText)
	}
}

func TestSendGatewayStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	err := SendGateway(context.Background(), GatewayConfig{
		URLTemplate:  server.URL + "?phone={phone}&text={text}",
		AllowPrivate: true,
	}, "13800138000", "测试")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("非 2xx 应该报错: %v", err)
	}
}

func TestSendGatewayBlocksPrivateByDefault(t *testing.T) {
	err := SendGateway(context.Background(), GatewayConfig{
		URLTemplate: "http://127.0.0.1:8080/send?phone={phone}&text={text}",
	}, "13800138000", "测试")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("默认应该要求 https: %v", err)
	}
	err = SendGateway(context.Background(), GatewayConfig{
		URLTemplate: "https://127.0.0.1/send?phone={phone}&text={text}",
	}, "13800138000", "测试")
	if err == nil || !strings.Contains(err.Error(), "内网") {
		t.Fatalf("默认应该拒绝内网地址: %v", err)
	}
}
