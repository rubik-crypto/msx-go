// Package aliyunvoice 通过阿里云语音通知服务拨打语音电话，并播报审核过的模板内容。
//
// 语音电话适合在收到重要事件时主动提醒用户。阿里云要求先在控制台创建文本转
// 语音模板并审核通过，发送时只传模板编号和模板变量，服务端无法临时拼接正文。
//
// 典型用法：
//
//	client := aliyunvoice.New(nil)
//	_, err := client.Call(ctx, aliyunvoice.Config{
//		AccessKeyID:     "你的 AccessKey ID",
//		AccessKeySecret: "你的 AccessKey Secret",
//		TtsCode:         "TTS_123456789",
//		CalledNumber:    "13800000000",
//	})
//	if err != nil {
//		// 错误信息适合直接展示给用户，不包含密钥。
//	}
package aliyunvoice

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	endpoint = "https://dyvmsapi.aliyuncs.com/"
	version  = "2017-05-25"
	action   = "SingleCallByTts"
)

var (
	keyIDExpr    = regexp.MustCompile(`^[A-Za-z0-9]{6,64}$`)
	ttsCodeExpr  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	phoneExpr    = regexp.MustCompile(`^\+?[0-9]{7,20}$`)
	paramKeyExpr = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
)

// Config 是一通语音电话需要的配置。只有模板里定义了变量时才需要 TemplateParams。
type Config struct {
	AccessKeyID      string
	AccessKeySecret  string
	TtsCode          string
	CalledNumber     string
	CalledShowNumber string // 可选：控制台申请的外呼号码
	TemplateParams   map[string]string
}

// Result 是语音服务受理成功后的回执。
type Result struct {
	CallID    string
	RequestID string
}

// Validate 检查配置是否完整、格式是否明显不对。它不发起网络请求。
func (c Config) Validate() error {
	keyID := strings.TrimSpace(c.AccessKeyID)
	secret := strings.TrimSpace(c.AccessKeySecret)
	code := strings.TrimSpace(c.TtsCode)
	phone := NormalizePhone(c.CalledNumber)
	if keyID == "" || secret == "" || code == "" || phone == "" {
		return errors.New("语音电话配置不完整，需要 AccessKey、模板编号和接听号码")
	}
	if !keyIDExpr.MatchString(keyID) {
		return errors.New("AccessKey ID 格式不正确")
	}
	if len(secret) < 8 || len(secret) > 128 || strings.ContainsAny(secret, " \t\r\n") {
		return errors.New("AccessKey Secret 格式不正确")
	}
	if !ttsCodeExpr.MatchString(code) {
		return errors.New("语音模板编号格式不正确")
	}
	if !phoneExpr.MatchString(phone) {
		return errors.New("接听号码格式不正确")
	}
	if caller := NormalizePhone(c.CalledShowNumber); caller != "" && !phoneExpr.MatchString(caller) {
		return errors.New("外呼号码格式不正确")
	}
	if len(c.TemplateParams) > 20 {
		return errors.New("语音模板变量太多")
	}
	for key, value := range c.TemplateParams {
		if !paramKeyExpr.MatchString(key) {
			return errors.New("语音模板变量名格式不正确")
		}
		if len(value) > 64 {
			return errors.New("语音模板变量内容太长")
		}
	}
	return nil
}

// NormalizePhone 去掉用户输入里常见的空格、横线和括号。
func NormalizePhone(raw string) string {
	s := strings.TrimSpace(raw)
	return strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(s)
}

// Client 负责发起语音电话。并发安全，可以复用。
type Client struct {
	http  *http.Client
	now   func() time.Time
	nonce func() (string, error)
}

// New 创建客户端。client 为 nil 时使用内置客户端：20 秒超时并拒绝跳转。
func New(client *http.Client) *Client {
	if client == nil {
		client = &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Client{
		http:  client,
		now:   time.Now,
		nonce: randomNonce,
	}
}

// Call 拨打一通语音电话。返回的错误不包含 AccessKey Secret，可以直接展示给用户。
func (c *Client) Call(ctx context.Context, cfg Config) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	nonce, err := c.nonce()
	if err != nil {
		return Result{}, errors.New("生成语音请求编号失败，请稍后重试")
	}
	req, err := c.buildRequest(ctx, cfg, c.now(), nonce)
	if err != nil {
		return Result{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, errors.New("语音电话请求超时，请稍后重试")
		}
		return Result{}, errors.New("连不上语音服务，请检查网络后重试")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return Result{}, errors.New("语音服务返回内容读取失败")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Result{}, errors.New("语音服务拒绝了密钥，请检查 AccessKey 是否正确")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("语音服务返回 HTTP %d，请稍后重试", resp.StatusCode)
	}
	var payload struct {
		Code      string `json:"Code"`
		Message   string `json:"Message"`
		RequestID string `json:"RequestId"`
		CallID    string `json:"CallId"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Result{}, errors.New("语音服务返回内容看不懂，请稍后重试")
	}
	if payload.Code != "OK" {
		message := strings.TrimSpace(payload.Message)
		if message == "" {
			message = strings.TrimSpace(payload.Code)
		}
		if message == "" {
			message = "未知原因"
		}
		return Result{}, fmt.Errorf("语音电话没有拨出去：%s", truncate(message, 120))
	}
	return Result{CallID: payload.CallID, RequestID: payload.RequestID}, nil
}

func (c *Client) buildRequest(ctx context.Context, cfg Config, now time.Time, nonce string) (*http.Request, error) {
	params := map[string]string{
		"AccessKeyId":      strings.TrimSpace(cfg.AccessKeyID),
		"Action":           action,
		"Format":           "JSON",
		"RegionId":         "cn-hangzhou",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   nonce,
		"SignatureVersion": "1.0",
		"Timestamp":        now.UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          version,
		"CalledNumber":     NormalizePhone(cfg.CalledNumber),
		"TtsCode":          strings.TrimSpace(cfg.TtsCode),
	}
	if caller := NormalizePhone(cfg.CalledShowNumber); caller != "" {
		params["CalledShowNumber"] = caller
	}
	if len(cfg.TemplateParams) > 0 {
		encoded, err := json.Marshal(cfg.TemplateParams)
		if err != nil {
			return nil, errors.New("语音模板变量整理失败")
		}
		params["TtsParam"] = string(encoded)
	}
	params["Signature"] = signature(params, strings.TrimSpace(cfg.AccessKeySecret), http.MethodPost)

	form := url.Values{}
	for key, value := range params {
		form.Set(key, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("语音电话请求创建失败")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	return req, nil
}

func signature(params map[string]string, secret, method string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		if key == "Signature" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for i, key := range keys {
		if i > 0 {
			canonical.WriteByte('&')
		}
		canonical.WriteString(percentEncode(key))
		canonical.WriteByte('=')
		canonical.WriteString(percentEncode(params[key]))
	}
	stringToSign := method + "&" + percentEncode("/") + "&" + percentEncode(canonical.String())
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func percentEncode(value string) string {
	encoded := url.QueryEscape(value)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "*", "%2A")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	if cut <= 0 {
		return ""
	}
	return s[:cut] + "…"
}
