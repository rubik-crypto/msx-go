package pushnotify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func fakeClient(status int, body string, captured ...*http.Request) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if len(captured) > 0 {
			*captured[0] = *req
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
}

func TestSendBark(t *testing.T) {
	var got http.Request
	client := fakeClient(200, `{"code":200,"message":"success"}`, &got)
	sender := New(client)
	cfg := Config{Channel: Bark, BarkKey: "https://api.day.app/abcd1234/标题/正文", BarkGroup: "测试分组"}
	if err := sender.Send(context.Background(), cfg, Message{Title: "标题", Body: "正文"}); err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	if got.URL.Path != "/push" || got.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("Bark 请求不对：%s %s", got.URL.Path, got.Header.Get("Content-Type"))
	}
	var payload map[string]any
	body, _ := io.ReadAll(got.Body)
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	if payload["device_key"] != "abcd1234" || payload["title"] != "标题" || payload["body"] != "正文" || payload["group"] != "测试分组" {
		t.Fatalf("Bark 请求体不对：%v", payload)
	}
}

func TestSendBarkRejected(t *testing.T) {
	client := fakeClient(200, `{"code":400,"message":"invalid device token"}`)
	err := New(client).Send(context.Background(), Config{Channel: Bark, BarkKey: "abcd"}, Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "invalid device token") {
		t.Fatalf("应该返回 Bark 的错误信息：%v", err)
	}
}

func TestSendServerChan(t *testing.T) {
	var got http.Request
	client := fakeClient(200, `{"code":0,"message":""}`, &got)
	err := New(client).Send(context.Background(), Config{Channel: ServerChan, ServerChanKey: "SCT123"}, Message{Title: "标题", Body: "正文"})
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	if got.URL.Path != "/SCT123.send" || got.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("Server酱请求不对：%s %s", got.URL.Path, got.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(got.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("title") != "标题" || form.Get("desp") != "正文" {
		t.Fatalf("Server酱表单不对：%q %v", body, err)
	}
}

func TestSendWebhook(t *testing.T) {
	var got http.Request
	client := fakeClient(200, `{"errcode":0,"errmsg":"ok"}`, &got)
	err := New(client).Send(context.Background(), Config{
		Channel:    Webhook,
		WebhookURL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc",
	}, Message{Title: "标题", Body: "正文"})
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	var payload map[string]any
	body, _ := io.ReadAll(got.Body)
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	text, _ := payload["text"].(map[string]any)
	content, _ := payload["content"].(map[string]any)
	if payload["msgtype"] != "text" || text["content"] != "标题\n正文" {
		t.Fatalf("企微/钉钉字段不对：%v", payload)
	}
	if payload["msg_type"] != "text" || content["text"] != "标题\n正文" {
		t.Fatalf("飞书字段不对：%v", payload)
	}
}

func TestWebhookFailure(t *testing.T) {
	client := fakeClient(200, `{"errcode":93000,"errmsg":"invalid webhook url"}`)
	err := New(client).Send(context.Background(), Config{
		Channel:    Webhook,
		WebhookURL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc",
	}, Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "invalid webhook url") {
		t.Fatalf("应该返回群机器人的错误信息：%v", err)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	client := fakeClient(401, `{}`)
	err := New(client).Send(context.Background(), Config{Channel: Bark, BarkKey: "abcd"}, Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("HTTP 状态错误没有体现：%v", err)
	}
}

func TestParseBarkTarget(t *testing.T) {
	base, key, err := ParseBarkTarget("https://api.day.app/abcd1234/标题/正文")
	if err != nil || base != "https://api.day.app" || key != "abcd1234" {
		t.Fatalf("解析完整地址失败：%q %q %v", base, key, err)
	}
	base, key, err = ParseBarkTarget(" abcd1234 ")
	if err != nil || base != "https://api.day.app" || key != "abcd1234" {
		t.Fatalf("解析密钥失败：%q %q %v", base, key, err)
	}
	if _, _, err := ParseBarkTarget("http://api.day.app/abcd"); err == nil {
		t.Fatal("http 地址应该被拒绝")
	}
	if _, _, err := ParseBarkTarget("https://localhost/abcd"); err == nil {
		t.Fatal("本机地址应该被拒绝")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	bad := []string{
		"",
		"http://example.com/hook",
		"https://127.0.0.1/hook",
		"https://localhost/hook",
		"https://10.0.0.8/hook",
		"https://192.168.1.10/hook",
		"https://[::1]/hook",
		"https://user:pass@example.com/hook",
	}
	for _, raw := range bad {
		if err := ValidateWebhookURL(raw); err == nil {
			t.Fatalf("应该拒绝 %q", raw)
		}
	}
	if err := ValidateWebhookURL("https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc"); err != nil {
		t.Fatalf("公网地址被误拒绝：%v", err)
	}
}

func TestConfigConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Fatal("空配置不应算已配置")
	}
	if !(Config{Channel: Bark, BarkKey: "abc"}).Configured() {
		t.Fatal("Bark 配置没有识别出来")
	}
	if !(Config{Channel: ServerChan, ServerChanKey: "abc"}).Configured() {
		t.Fatal("Server酱配置没有识别出来")
	}
	if !(Config{Channel: Webhook, WebhookURL: "https://example.com"}).Configured() {
		t.Fatal("Webhook 配置没有识别出来")
	}
}
