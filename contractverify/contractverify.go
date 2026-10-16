// Package contractverify 把已编译好的合约源码提交给区块浏览器做验证，并查询验证结果。
//
// 解决的问题：EVM 链上的合约默认不开源，想让浏览器和钱包认出它，得把源码连同编译设置
// 提交给浏览器，由浏览器重新编译一遍、和链上的字节码逐字节比对；比对通过才算「已验证」。
// 各家浏览器的接口长得很像（都源自 Etherscan 那一套，接口路径和参数名基本通用），
// 本包把「提交 + 查结果」这两步封好，包括那个从很多年前就拼错的参数名。
//
// 提交要带上当初编译用的全部设置，而这些设置没有权威来源——只有原合约验证时用的那一份。
// 所以正确的做法是：把原合约那份源码和设置原样取出来，改掉合约地址和构造参数再提交一次，
// 而不是自己另拼一套。BuildStandardJSONInput 负责把源码和设置拼成标准 JSON 输入。
//
//	c := &contractverify.Client{APIKey: key}
//	guid, err := c.Submit(ctx, 56, contractverify.Request{
//	    Address:         newContract,
//	    CompilerVersion: "v0.8.20+commit.a1b79de6",
//	    Format:          contractverify.FormatStandardJSON,
//	    ContractName:    "contracts/Token.sol:Token",
//	    SourceCode:      standardJSON, // BuildStandardJSONInput 的产物
//	    ConstructorArgs: argsHex,      // 十六进制，不带 0x
//	})
//	st, err := c.Status(ctx, 56, guid)
//	if st.State == contractverify.StatePass { /* 通过了 */ }
//
// 接口密钥是必须的（浏览器不给免密钥的提交入口）。查询结果大多是异步的：
// 刚提交时返回 StatePending，要过一会儿再问。
package contractverify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint 是 Etherscan 系接口的统一入口：链的编号放在 chainid 参数里，
// 一个密钥可查多条链。自建浏览器（Blockscout 这类）的路径不一样，用 Client.Endpoint 覆盖。
const DefaultEndpoint = "https://api.etherscan.io/v2/api"

// Format 是源码的提交形式，决定浏览器怎么编译它。
type Format string

const (
	// FormatStandardJSON 标准 JSON 输入：多文件合约、或带了完整编译设置时用这个。
	FormatStandardJSON Format = "solidity-standard-json-input"
	// FormatSingleFile 单个源码文件：源码已经被拍平成一个文件时用这个。
	FormatSingleFile Format = "solidity-single-file"
)

// State 是验证进度。
type State string

const (
	// StatePending 浏览器还在排队或正在编译比对。
	StatePending State = "pending"
	// StatePass 验证通过。
	StatePass State = "pass"
	// StateFail 验证没通过，具体原因看 Status.Raw。
	StateFail State = "fail"
)

// Client 是提交验证用的客户端。零值不可用，至少要有 APIKey。
type Client struct {
	// APIKey 区块浏览器的接口密钥。
	APIKey string
	// Endpoint 接口入口，留空用 DefaultEndpoint。
	Endpoint string
	// HTTP 留空用带超时的默认客户端。
	HTTP *http.Client
}

// Request 是一次源码验证提交所需的全部内容。
type Request struct {
	// Address 要验证的合约地址（链上已经部署的那个）。
	Address string
	// CompilerVersion 当初编译用的版本，形如 v0.8.20+commit.a1b79de6。
	// 必须和实际编译时完全一致，可以用 NormalizeCompilerVersion 统一格式。
	CompilerVersion string
	// Format 提交形式；留空按标准 JSON 处理。
	Format Format
	// SourceCode 源码内容：Format 是标准 JSON 时放整份 JSON，单文件时放拍平后的源码。
	SourceCode string
	// ContractName 合约名：标准 JSON 时是「文件名:合约名」，单文件时是合约名本身。
	ContractName string
	// ConstructorArgs 部署时填的构造参数，十六进制、不带 0x；没有参数就留空。
	ConstructorArgs string
	// OptimizerEnabled 当初有没有开优化；FormatSingleFile 时要填对。
	OptimizerEnabled bool
	// OptimizerRuns 优化轮数，OptimizerEnabled 为真时才有意义。
	OptimizerRuns int
	// EVMVersion 编译时的 EVM 版本，例如 paris；不确定就留空，让编译器用默认值。
	EVMVersion string
	// LicenseType 开源许可编号，见 LicenseType；0 表示不声明。
	LicenseType int
	// Libraries 用到的库：库名 → 地址。单文件形式靠它告诉浏览器库地址。
	Libraries map[string]string
}

// Status 是一次查询的结果。
type Status struct {
	State State
	// Raw 浏览器原样回的那句话，认不出原因时可以展示给用户。
	Raw string
}

// APIError 表示浏览器明确拒绝了这次请求（密钥不对、参数不对、已经验证过等）。
type APIError struct {
	// Message 浏览器给的说明。
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Submit 提交一次源码验证，成功时返回查询要用的编号。
func (c *Client) Submit(ctx context.Context, chainID int, req Request) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", errors.New("contractverify: 缺接口密钥，区块浏览器不接受无密钥的提交")
	}
	if strings.TrimSpace(req.Address) == "" {
		return "", errors.New("contractverify: 缺合约地址")
	}
	if strings.TrimSpace(req.SourceCode) == "" {
		return "", errors.New("contractverify: 缺源码内容")
	}
	if strings.TrimSpace(req.ContractName) == "" {
		return "", errors.New("contractverify: 缺合约名；标准 JSON 形式要写「文件名:合约名」")
	}
	if strings.TrimSpace(req.CompilerVersion) == "" {
		return "", errors.New("contractverify: 缺编译器版本号")
	}

	form := url.Values{}
	form.Set("module", "contract")
	form.Set("action", "verifysourcecode")
	form.Set("contractaddress", strings.TrimSpace(req.Address))
	form.Set("compilerversion", NormalizeCompilerVersion(req.CompilerVersion))
	form.Set("contractname", strings.TrimSpace(req.ContractName))
	form.Set("sourceCode", req.SourceCode)
	format := req.Format
	if format == "" {
		format = FormatStandardJSON
	}
	form.Set("codeformat", string(format))
	if args := strings.TrimPrefix(strings.TrimSpace(req.ConstructorArgs), "0x"); args != "" {
		// 接口文档里这个参数名的拼写错了（Arguements），但一直就是按这个收的；
		// 顺手把正确拼写也带上，两边都不吃亏。
		form.Set("constructorArguements", args)
		form.Set("constructorArguments", args)
	}
	if format == FormatSingleFile {
		if req.OptimizerEnabled {
			form.Set("optimizationUsed", "1")
			runs := req.OptimizerRuns
			if runs <= 0 {
				runs = 200
			}
			form.Set("runs", strconv.Itoa(runs))
		} else {
			form.Set("optimizationUsed", "0")
		}
		if v := NormalizeEVMVersion(req.EVMVersion); v != "" {
			form.Set("evmversion", v)
		}
		if req.LicenseType > 0 {
			form.Set("licenseType", strconv.Itoa(req.LicenseType))
		}
		names := make([]string, 0, len(req.Libraries))
		for name := range req.Libraries {
			names = append(names, name)
		}
		sort.Strings(names) // 顺序固定，同样的输入发出同样的请求，便于排查
		for i, name := range names {
			n := strconv.Itoa(i + 1)
			form.Set("libraryname"+n, name)
			form.Set("libraryaddress"+n, req.Libraries[name])
		}
	}

	m, err := c.call(ctx, chainID, form)
	if err != nil {
		return "", err
	}
	result := pickString(m, "result")
	if pickString(m, "status") == "1" && result != "" {
		return result, nil
	}
	if result == "" {
		result = pickString(m, "message")
	}
	if result == "" {
		result = "浏览器没有说明原因"
	}
	return "", &APIError{Message: result}
}

// Status 查一次验证进度。
func (c *Client) Status(ctx context.Context, chainID int, guid string) (Status, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return Status{}, errors.New("contractverify: 缺接口密钥")
	}
	guid = strings.TrimSpace(guid)
	if guid == "" {
		return Status{}, errors.New("contractverify: 缺验证编号")
	}
	form := url.Values{}
	form.Set("module", "contract")
	form.Set("action", "checkverifystatus")
	form.Set("guid", guid)
	m, err := c.call(ctx, chainID, form)
	if err != nil {
		return Status{}, err
	}
	raw := pickString(m, "result")
	if raw == "" {
		raw = pickString(m, "message")
	}
	return Status{State: ClassifyStatus(pickString(m, "status"), raw), Raw: raw}, nil
}

// call 发一个表单请求，把返回值当 JSON 解出来。
func (c *Client) call(ctx context.Context, chainID int, form url.Values) (map[string]any, error) {
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	form.Set("apikey", c.APIKey)
	u := endpoint
	if strings.Contains(u, "?") {
		u += "&chainid=" + strconv.Itoa(chainID)
	} else {
		u += "?chainid=" + strconv.Itoa(chainID)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("contractverify: 连不上区块浏览器接口：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("contractverify: 读不到接口返回的内容：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("contractverify: 接口返回 %d：%s", resp.StatusCode, clip(string(raw), 160))
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, errors.New("contractverify: 接口返回的内容不是合法 JSON，接口地址可能变了")
	}
	return out, nil
}

// ClassifyStatus 把浏览器的状态和那句话分成三档。
// 浏览器在不同阶段给的话不完全一样（有时 status 是 0 但仍在排队），所以按原话判更准。
func ClassifyStatus(status, raw string) State {
	l := strings.ToLower(raw)
	switch {
	case strings.Contains(l, "already verified"):
		return StatePass
	case strings.Contains(l, "pending"), strings.Contains(l, "in queue"),
		strings.Contains(l, "in progress"), strings.Contains(l, "processing"):
		return StatePending
	case strings.Contains(l, "pass"):
		return StatePass
	case strings.Contains(l, "fail"), strings.Contains(l, "unable"),
		strings.Contains(l, "does not match"), strings.Contains(l, "error"):
		return StateFail
	}
	if strings.TrimSpace(status) == "1" {
		return StatePass
	}
	return StatePending
}

// NormalizeCompilerVersion 把各家给的编译器版本号统一成浏览器要的形状：
// 一定要有 v 前缀，后面带 +commit. 那段（例如 v0.8.20+commit.a1b79de6）。
// 有的来源写成 "0.8.20+commit.a1b79de6"，有的写成 "solc: v0.8.20+..."。
func NormalizeCompilerVersion(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if v := strings.TrimSpace(s[i+1:]); v != "" {
			s = v
		}
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "solc"))
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "v") && !strings.HasPrefix(s, "V") {
		s = "v" + s
	}
	return s
}

// NormalizeEVMVersion 只在确定有意义时才返回 EVM 版本：各家常写成 default/空，那种就交给编译器默认。
func NormalizeEVMVersion(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "default", "compiler default", "latest":
		return ""
	}
	return s
}

// LicenseType 把许可名字翻成浏览器要的编号。认不出来返回 1（不声明许可）。
// 编号对照：1 无、2 Unlicense、3 MIT、4 GPL-2.0、5 GPL-3.0、6 AGPL-3.0、
// 7 LGPL-2.1、8 LGPL-3.0、9 BSD-2-Clause、10 BSD-3-Clause、11 MPL-2.0、
// 12 OSL-3.0、13 Apache-2.0。
func LicenseType(name string) int {
	k := strings.ToLower(strings.TrimSpace(name))
	k = strings.ReplaceAll(k, "_", "-")
	k = strings.ReplaceAll(k, " ", "")
	k = strings.TrimSuffix(k, "license")
	k = strings.Trim(k, "-")
	switch k {
	case "", "none", "no", "unknown":
		return 1
	case "unlicense", "theunlicense":
		return 2
	case "mit":
		return 3
	case "gpl-2.0", "gplv2", "gnu-gpl-v2.0", "gnugplv2.0", "gpl-2.0-only", "gpl-2.0-or-later":
		return 4
	case "gpl-3.0", "gplv3", "gnu-gpl-v3.0", "gnugplv3.0", "gpl-3.0-only", "gpl-3.0-or-later":
		return 5
	case "agpl-3.0", "gnu-agpl-v3.0", "gnuagplv3.0", "agpl-3.0-only":
		return 6
	case "lgpl-2.1", "gnu-lgpl-v2.1", "gnulgplv2.1":
		return 7
	case "lgpl-3.0", "gnu-lgpl-v3.0", "gnulgplv3.0":
		return 8
	case "bsd-2-clause":
		return 9
	case "bsd-3-clause":
		return 10
	case "mpl-2.0":
		return 11
	case "osl-3.0":
		return 12
	case "apache-2.0", "apache":
		return 13
	}
	return 1
}

func pickString(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
