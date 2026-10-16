package contractverify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeExplorer 起一个假接口，按真实接口的形状回话。
func fakeExplorer(t *testing.T, response string, got *url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("请求不是表单：%v", err)
		}
		if got != nil {
			*got = r.PostForm
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
}

func TestSubmitFillsFormForStandardJSON(t *testing.T) {
	var got url.Values
	srv := fakeExplorer(t, `{"status":"1","message":"OK","result":"guid-123"}`, &got)
	defer srv.Close()

	c := &Client{APIKey: "KEY", Endpoint: srv.URL}
	guid, err := c.Submit(context.Background(), 56, Request{
		Address:         "0x1111111111111111111111111111111111111111",
		CompilerVersion: "0.8.20+commit.a1b79de6",
		Format:          FormatStandardJSON,
		ContractName:    "contracts/Token.sol:Token",
		SourceCode:      `{"language":"Solidity"}`,
		ConstructorArgs: "0xabcd",
	})
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if guid != "guid-123" {
		t.Errorf("编号不对：%q", guid)
	}
	if got.Get("contractaddress") != "0x1111111111111111111111111111111111111111" {
		t.Errorf("合约地址没带上：%v", got.Get("contractaddress"))
	}
	if got.Get("codeformat") != string(FormatStandardJSON) {
		t.Errorf("提交形式不对：%q", got.Get("codeformat"))
	}
	if got.Get("compilerversion") != "v0.8.20+commit.a1b79de6" {
		t.Errorf("编译器版本应该补上 v 前缀：%q", got.Get("compilerversion"))
	}
	if got.Get("contractname") != "contracts/Token.sol:Token" {
		t.Errorf("合约名没带上：%q", got.Get("contractname"))
	}
	// 接口那边这个参数名的拼写是错的，但一直按这个收，所以两个都得带
	if got.Get("constructorArguements") != "abcd" || got.Get("constructorArguments") != "abcd" {
		t.Errorf("构造参数应该去掉 0x 前缀并带上：%v", got)
	}
	if got.Get("apikey") != "KEY" {
		t.Errorf("密钥没带上：%q", got.Get("apikey"))
	}
	if got.Get("action") != "verifysourcecode" {
		t.Errorf("动作不对：%q", got.Get("action"))
	}
}

func TestSubmitFillsFormForSingleFile(t *testing.T) {
	var got url.Values
	srv := fakeExplorer(t, `{"status":"1","message":"OK","result":"g"}`, &got)
	defer srv.Close()

	c := &Client{APIKey: "KEY", Endpoint: srv.URL}
	_, err := c.Submit(context.Background(), 1, Request{
		Address:          "0x2222222222222222222222222222222222222222",
		CompilerVersion:  "v0.4.24+commit.e67f0147",
		Format:           FormatSingleFile,
		ContractName:     "Token",
		SourceCode:       "contract Token {}",
		OptimizerEnabled: true,
		OptimizerRuns:    333,
		EVMVersion:       "default",
		LicenseType:      3,
		Libraries:        map[string]string{"SafeMath": "0x3333333333333333333333333333333333333333"},
	})
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if got.Get("codeformat") != string(FormatSingleFile) {
		t.Errorf("提交形式不对：%q", got.Get("codeformat"))
	}
	if got.Get("optimizationUsed") != "1" || got.Get("runs") != "333" {
		t.Errorf("优化设置没带上：%v %v", got.Get("optimizationUsed"), got.Get("runs"))
	}
	if got.Get("evmversion") != "" {
		t.Errorf("default 这种没意义的 EVM 版本不该带上：%q", got.Get("evmversion"))
	}
	if got.Get("licenseType") != "3" {
		t.Errorf("许可编号没带上：%q", got.Get("licenseType"))
	}
	if got.Get("libraryname1") != "SafeMath" || got.Get("libraryaddress1") != "0x3333333333333333333333333333333333333333" {
		t.Errorf("库地址没带上：%v", got)
	}
}

func TestSubmitReportsExplorerRejection(t *testing.T) {
	srv := fakeExplorer(t, `{"status":"0","message":"NOTOK","result":"Contract source code already verified"}`, nil)
	defer srv.Close()

	c := &Client{APIKey: "KEY", Endpoint: srv.URL}
	_, err := c.Submit(context.Background(), 56, Request{
		Address:         "0x1111111111111111111111111111111111111111",
		CompilerVersion: "v0.8.20+commit.a1b79de6",
		ContractName:    "contracts/Token.sol:Token",
		SourceCode:      "{}",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应该返回 *APIError，实际 %v", err)
	}
	if apiErr.Message != "Contract source code already verified" {
		t.Errorf("应该把浏览器原话带出来：%q", apiErr.Message)
	}
}

func TestSubmitRejectsMissingInput(t *testing.T) {
	full := func() Request {
		return Request{
			Address:         "0x1111111111111111111111111111111111111111",
			CompilerVersion: "v0.8.20+commit.a1b79de6",
			ContractName:    "contracts/Token.sol:Token",
			SourceCode:      "{}",
		}
	}
	// 地址故意指向一个连不上的端口：真发出去就会报「连不上」，和「参数不全」区分得开
	client := &Client{APIKey: "KEY", Endpoint: "http://127.0.0.1:1/api"}

	noAddress := full()
	noAddress.Address = ""
	noSource := full()
	noSource.SourceCode = ""
	noName := full()
	noName.ContractName = ""
	noCompiler := full()
	noCompiler.CompilerVersion = ""

	type tc struct {
		name string
		c    *Client
		req  Request
	}
	cases := []tc{
		{"没有密钥", &Client{Endpoint: client.Endpoint}, full()},
		{"没有地址", client, noAddress},
		{"没有源码", client, noSource},
		{"没有合约名", client, noName},
		{"没有编译器版本", client, noCompiler},
	}
	for _, c := range cases {
		if _, err := c.c.Submit(context.Background(), 56, c.req); err == nil {
			t.Errorf("%s：应该直接报错，不该发请求", c.name)
		} else if strings.Contains(err.Error(), "连不上") {
			t.Errorf("%s：应该在发请求之前就拦住，实际报的是 %v", c.name, err)
		}
	}
}

func TestStatusParsesState(t *testing.T) {
	srv := fakeExplorer(t, `{"status":"1","message":"OK","result":"Pass - Verified"}`, nil)
	defer srv.Close()
	c := &Client{APIKey: "KEY", Endpoint: srv.URL}
	st, err := c.Status(context.Background(), 56, "guid-1")
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if st.State != StatePass || !strings.Contains(st.Raw, "Pass") {
		t.Errorf("结果判读不对：%+v", st)
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status, raw string
		want        State
	}{
		{"1", "Pass - Verified", StatePass},
		{"0", "Pending in queue", StatePending},
		{"0", "Fail - Unable to verify", StateFail},
		{"0", "Contract source code already verified", StatePass},
		{"0", "", StatePending},
	}
	for _, c := range cases {
		if got := ClassifyStatus(c.status, c.raw); got != c.want {
			t.Errorf("ClassifyStatus(%q,%q) = %q，想要 %q", c.status, c.raw, got, c.want)
		}
	}
}

func TestNormalizeCompilerVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.8.20+commit.a1b79de6", "v0.8.20+commit.a1b79de6"},
		{"v0.8.20+commit.a1b79de6", "v0.8.20+commit.a1b79de6"},
		{"solc: v0.5.16+commit.9c3226ce", "v0.5.16+commit.9c3226ce"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeCompilerVersion(c.in); got != c.want {
			t.Errorf("NormalizeCompilerVersion(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

func TestLicenseType(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 1}, {"None", 1}, {"MIT", 3}, {"Apache-2.0", 13},
		{"gnu-gpl-v3.0", 5}, {"GPL-3.0", 5}, {"BSD-3-Clause", 10}, {"认不出来的许可", 1},
	}
	for _, c := range cases {
		if got := LicenseType(c.in); got != c.want {
			t.Errorf("LicenseType(%q) = %d，想要 %d", c.in, got, c.want)
		}
	}
}

func TestBuildStandardJSONInput(t *testing.T) {
	sources := []Source{
		{Path: "contracts/Token.sol", Content: "contract Token {}"},
		{Path: "contracts/Lib.sol", Content: "library Lib {}"},
	}
	settings := map[string]any{
		"optimizer":         map[string]any{"enabled": true, "runs": float64(200)},
		"compilationTarget": map[string]any{"contracts/Token.sol": "Token"}, // 输出信息，必须丢掉
		"evmVersion":        "paris",
		"libraries":         map[string]any{},
		"remappings":        []any{},
		"outputSelection":   nil, // 占位，下面会删掉
	}
	delete(settings, "outputSelection")

	out, err := BuildStandardJSONInput(sources, settings, "")
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	var obj struct {
		Language string `json:"language"`
		Sources  map[string]struct {
			Content string `json:"content"`
		} `json:"sources"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("不是合法 JSON：%v", err)
	}
	if obj.Language != "Solidity" {
		t.Errorf("语言默认应该是 Solidity：%q", obj.Language)
	}
	if _, ok := obj.Sources["contracts/Token.sol"]; !ok {
		t.Error("源码路径必须原样保留")
	}
	if _, ok := obj.Settings["compilationTarget"]; ok {
		t.Error("compilationTarget 是输出信息，不该留在输入设置里")
	}
	if _, ok := obj.Settings["outputSelection"]; !ok {
		t.Error("应该补上默认的 outputSelection")
	}
	if obj.Settings["evmVersion"] != "paris" {
		t.Errorf("原设置应该保留：%v", obj.Settings["evmVersion"])
	}

	if _, err := BuildStandardJSONInput(nil, nil, ""); err == nil {
		t.Error("没有源码文件时应该报错")
	}
}

func TestSourcesFromStandardJSON(t *testing.T) {
	inner := `{"language":"Solidity","sources":{"a.sol":{"content":"contract A {}"}},"settings":{"evmVersion":"london"}}`
	got, ok := SourcesFromStandardJSON("{" + inner + "}")
	if !ok || len(got) != 1 || got[0].Path != "a.sol" {
		t.Fatalf("多包一层大括号的应该能解出来：%v %v", ok, got)
	}
	if s := SettingsFromStandardJSON("{" + inner + "}"); s == nil || s["evmVersion"] != "london" {
		t.Errorf("编译设置没解出来：%v", s)
	}
	if _, ok := SourcesFromStandardJSON("contract A {}"); ok {
		t.Error("拍平的源码不该被当成标准 JSON")
	}
}
