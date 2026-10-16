// Package contractsource 从区块浏览器读取已部署合约的源码、ABI 与创建字节码。
//
// 解决的问题：要复制、再部署或者审计一个链上合约，先得把它的 ABI 和「创建字节码」
// 拿到手。可同一份资料散落在三处——开源的区块浏览器接口、公开源码仓库、
// 商业浏览器的 API——字段名各叫各的，只问一家经常拿不全或者直接查不到。
// 本包把三处都问一遍再合并，顺手分清「编译器输出的干净创建代码」和
// 「链上那笔创建交易里粘着构造参数的原始代码」——后者直接拿去部署会把旧参数带进去。
//
//	c := contractsource.Chain{ChainID: 1, BlockscoutHost: "https://eth.blockscout.com"}
//	info, err := contractsource.Fetch(ctx, c, "0xdAC17F958D2ee523a2206206994597C13D831ec7")
//	types, _ := info.ConstructorArgTypes()
//	code, args, ok := info.CleanCreationBytecode()
//
// 拿到的 code 直接拼上自己编码的构造参数，就是一笔部署交易的 data。
package contractsource

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rubik-crypto/msx-go/evmabi"
)

// ErrNotFound 表示这个地址在数据源里没有任何记录。
var ErrNotFound = errors.New("这个地址在这里没有资料")

var addressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// IsAddress 判断是不是一个合法的 0x 地址。
func IsAddress(s string) bool { return addressRe.MatchString(strings.TrimSpace(s)) }

// Chain 描述去哪条链上找资料。
type Chain struct {
	// ChainID 链 ID，例如以太坊 1、BNB 链 56。
	ChainID int64
	// BlockscoutHost 该链上可用的开源浏览器地址，例如 https://eth.blockscout.com，
	// 没有就留空。
	BlockscoutHost string
	// SourcifyBase 公开源码仓库的地址，留空用默认值。
	SourcifyBase string
	// EtherscanAPIKey 商业浏览器接口的密钥，留空表示不问这一家。
	EtherscanAPIKey string
	// EtherscanBase 商业浏览器接口地址，留空用默认值。
	EtherscanBase string
	// RPC 链上节点地址，用来在别的数据源只给了创建交易号时把原始数据取回来。
	RPC []string
	// HTTPClient 自定义 HTTP 客户端，留空用默认（20 秒超时）。
	HTTPClient *http.Client
	// Logf 出错时的日志输出，留空用标准库的 log.Printf。
	Logf func(format string, args ...any)
}

func (c Chain) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (c Chain) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultClient
}

var defaultClient = &http.Client{Timeout: 20 * time.Second}

// SourceFile 一个源码文件。
type SourceFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Info 是一个合约在区块浏览器上的全部资料。字段拿不到时是零值。
type Info struct {
	Name             string          `json:"name"`
	ABI              json.RawMessage `json:"abi"`
	Sources          []SourceFile    `json:"sources"`
	Compiler         string          `json:"compiler"`
	Language         string          `json:"language"`
	License          string          `json:"license"`
	CreationBytecode string          `json:"creationBytecode"` // 0x 开头
	DeployedBytecode string          `json:"deployedBytecode"` // 0x 开头
	ConstructorArgs  string          `json:"constructorArgs"`  // 0x 开头
	CreationTxHash   string          `json:"creationTxHash"`
	Proxy            bool            `json:"proxy"`
	Implementations  []string        `json:"implementations"`
	Verified         bool            `json:"verified"`
	Providers        []string        `json:"providers"`
	Warnings         []string        `json:"warnings"`

	// creationIsOnchain 表示手上的创建代码是链上那笔交易的原始数据，
	// 末尾很可能粘着当时填的构造参数。
	creationIsOnchain bool
}

// HasCreationBytecode 有没有拿到能用来部署的创建代码。
func (i *Info) HasCreationBytecode() bool { return i != nil && len(i.CreationBytecode) > 2 }

// CreationBytecodeFromChain 手上的创建代码是不是链上那笔交易的原始数据
// （末尾往往粘着当时填的构造参数）。
func (i *Info) CreationBytecodeFromChain() bool { return i != nil && i.creationIsOnchain }

// Fetch 按「开源浏览器 → 公开源码仓库 → 商业浏览器接口」的顺序问一遍，合并结果。
// 地址或链信息不对时直接返回错误；查不到时返回 ErrNotFound。
func Fetch(ctx context.Context, chain Chain, address string) (*Info, error) {
	address = strings.TrimSpace(address)
	if !IsAddress(address) {
		return nil, fmt.Errorf("合约地址 %q 不合法，应该是 0x 加 40 位十六进制", address)
	}

	type attempt struct {
		name string
		fn   func(context.Context) (*Info, error)
	}
	var attempts []attempt
	if host := strings.TrimSpace(chain.BlockscoutHost); host != "" {
		attempts = append(attempts, attempt{"开源区块浏览器", func(ctx context.Context) (*Info, error) {
			return fetchBlockscout(ctx, chain, host, address)
		}})
	}
	attempts = append(attempts, attempt{"公开源码仓库", func(ctx context.Context) (*Info, error) {
		return fetchSourcify(ctx, chain, address)
	}})
	if strings.TrimSpace(chain.EtherscanAPIKey) != "" {
		attempts = append(attempts, attempt{"区块链浏览器接口", func(ctx context.Context) (*Info, error) {
			return fetchEtherscan(ctx, chain, address)
		}})
	}

	var results []*Info
	var lastErr error
	for _, a := range attempts {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		info, err := a.fn(reqCtx)
		cancel()
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				lastErr = err
				chain.logf("读取合约失败（%s）：%v", a.name, err)
			}
			continue
		}
		if info == nil {
			continue
		}
		info.Providers = []string{a.name}
		results = append(results, info)
		if info.HasCreationBytecode() {
			break // 已经拿到能部署的东西，不必再问
		}
	}
	if len(results) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("没读到合约资料：%w", lastErr)
		}
		return nil, ErrNotFound
	}

	merged := mergeInfos(results)

	// 没有创建代码、但有创建交易号：去链上把那笔交易的原始数据取回来。
	if !merged.HasCreationBytecode() && merged.CreationTxHash != "" {
		code, err := fetchCreationInputFromRPC(ctx, chain, merged.CreationTxHash)
		if err != nil {
			chain.logf("从链上取创建代码失败：%v", err)
		} else if code != "" {
			merged.CreationBytecode = code
			merged.creationIsOnchain = true
			merged.Providers = append(merged.Providers, "链上交易数据")
		}
	}

	merged.finish()
	return merged, nil
}

// mergeInfos 先拿到的优先，缺的字段用后面的补上。
func mergeInfos(list []*Info) *Info {
	out := &Info{}
	for _, c := range list {
		if out.Name == "" {
			out.Name = c.Name
		}
		if out.ABI == nil && c.ABI != nil {
			out.ABI = c.ABI
		}
		if len(out.Sources) == 0 && len(c.Sources) > 0 {
			out.Sources = c.Sources
		}
		if out.Compiler == "" {
			out.Compiler = c.Compiler
		}
		if out.Language == "" {
			out.Language = c.Language
		}
		if out.License == "" {
			out.License = c.License
		}
		if out.CreationBytecode == "" && c.CreationBytecode != "" {
			out.CreationBytecode = c.CreationBytecode
			out.creationIsOnchain = c.creationIsOnchain
		}
		if out.DeployedBytecode == "" {
			out.DeployedBytecode = c.DeployedBytecode
		}
		if out.ConstructorArgs == "" {
			out.ConstructorArgs = c.ConstructorArgs
		}
		if out.CreationTxHash == "" {
			out.CreationTxHash = c.CreationTxHash
		}
		out.Verified = out.Verified || c.Verified
		out.Proxy = out.Proxy || c.Proxy
		out.Implementations = append(out.Implementations, c.Implementations...)
		out.Providers = append(out.Providers, c.Providers...)
	}
	return out
}

// finish 收尾：切掉链上创建代码末尾粘着的构造参数、整理源码顺序、写下注意事项。
// 可以重复调用。
func (i *Info) finish() {
	i.Warnings = nil
	if i.ABI != nil || len(i.Sources) > 0 {
		i.Verified = true
	}
	if len(i.Sources) > 0 {
		i.Sources = orderSources(i.Sources, i.Name)
	}

	types, err := i.ConstructorArgTypes()
	if err != nil {
		types = nil
	}

	// 创建代码末尾常常粘着部署时填的构造参数：能确定就切掉，单独放到 ConstructorArgs 里。
	switch {
	case i.CreationBytecode == "":
	case i.ConstructorArgs != "" && strings.HasSuffix(i.CreationBytecode, strings.TrimPrefix(i.ConstructorArgs, "0x")):
		// 数据源把参数单独给了、而且它就贴在末尾：按长度切，确定无疑。
		trimmed := strings.TrimPrefix(i.ConstructorArgs, "0x")
		i.CreationBytecode = i.CreationBytecode[:len(i.CreationBytecode)-len(trimmed)]
		i.creationIsOnchain = false
	case i.creationIsOnchain:
		if len(types) == 0 {
			// 切不了就留着标记，调用方还能自己判断要不要提醒用户
			i.Warnings = append(i.Warnings, "创建代码取自链上那笔交易，末尾可能粘着当时填的构造参数；因为没有读到 ABI，无法自动去掉。")
		} else if code, args, ok := i.CleanCreationBytecode(); ok {
			i.CreationBytecode = "0x" + code
			i.ConstructorArgs = "0x" + args
			i.creationIsOnchain = false
		}
	}

	if !i.Verified {
		i.Warnings = append(i.Warnings, "这个合约没有开源，看不到源码，只能拿到它的创建代码。")
	}
	if i.Proxy && len(i.Implementations) > 0 {
		i.Warnings = append(i.Warnings, "这是一个代理合约，直接复制部署只会得到一个空壳，真正的实现在 "+i.Implementations[0]+"。")
	} else if i.Proxy {
		i.Warnings = append(i.Warnings, "这个合约看起来是代理合约，直接复制部署通常没有意义。")
	} else if strings.Contains(strings.ToLower(i.Name), "proxy") {
		i.Warnings = append(i.Warnings, "合约名字里带 Proxy，很可能是个代理合约（空壳），请换成它背后真正干活的合约地址。")
	}
	if i.CreationBytecode == "" {
		i.Warnings = append(i.Warnings, "没有读到创建代码，这个合约可能没有开源；可以到区块浏览器的合约页复制「创建代码」再自行部署。")
	} else if i.DeployedBytecode != "" && len(i.CreationBytecode) <= len(i.DeployedBytecode) {
		i.Warnings = append(i.Warnings, "读到的创建代码不比运行代码长，可能拿到的不是创建代码，使用前请核对。")
	}
	if holes := LibraryPlaceholders(i.CreationBytecode); len(holes) > 0 {
		i.Warnings = append(i.Warnings, fmt.Sprintf("创建代码里有 %d 处库地址占位符，部署前必须填上对应库的地址。", len(holes)))
	}
}

// orderSources 把主文件排在最前面。
func orderSources(files []SourceFile, contractName string) []SourceFile {
	score := func(f SourceFile) int {
		switch {
		case contractName != "" && strings.EqualFold(baseName(f.Path), contractName+".sol"):
			return 0
		case contractName != "" && strings.Contains(f.Content, "contract "+contractName):
			return 1
		case strings.HasSuffix(strings.ToLower(f.Path), ".sol"):
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(files, func(a, b int) bool { return score(files[a]) < score(files[b]) })
	return files
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---------------------------------------------------------------- ABI 与类型

type abiArgSpec struct {
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	InternalType string       `json:"internalType,omitempty"`
	Components   []abiArgSpec `json:"components,omitempty"`
}

type abiEntry struct {
	Type   string       `json:"type"`
	Name   string       `json:"name"`
	Inputs []abiArgSpec `json:"inputs"`
}

// ParseABI 解析合约的 ABI；既支持 [ ... ] 这种数组，也支持 {"abi": [...]} 这种包装。
func ParseABI(raw []byte) ([]abiEntry, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var entries []abiEntry
	if err := json.Unmarshal(trimmed, &entries); err == nil {
		return entries, nil
	}
	var wrapped struct {
		ABI []abiEntry `json:"abi"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err == nil && wrapped.ABI != nil {
		return wrapped.ABI, nil
	}
	return nil, errors.New("ABI 内容看不懂，不是标准的合约接口描述")
}

// ConstructorArgTypes 取部署这个合约要填的参数类型（没有构造参数时返回空）。
func (i *Info) ConstructorArgTypes() ([]evmabi.Type, error) {
	entries, err := ParseABI(i.ABI)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.EqualFold(e.Type, "constructor") {
			continue
		}
		out := make([]evmabi.Type, 0, len(e.Inputs))
		for _, in := range e.Inputs {
			t, err := typeOfSpec(in)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, nil
	}
	return nil, nil
}

// typeOfSpec 把 ABI JSON 里的参数描述变成类型；元组要按 components 展开。
func typeOfSpec(s abiArgSpec) (evmabi.Type, error) {
	if !strings.HasPrefix(s.Type, "tuple") {
		return evmabi.ParseType(s.Type)
	}
	parts := make([]string, 0, len(s.Components))
	for _, c := range s.Components {
		inner, err := typeOfSpec(c)
		if err != nil {
			return evmabi.Type{}, err
		}
		parts = append(parts, inner.String())
	}
	return evmabi.ParseType("(" + strings.Join(parts, ",") + ")" + strings.TrimPrefix(s.Type, "tuple"))
}

// ---------------------------------------------------------------- 各家数据源

func fetchBlockscout(ctx context.Context, chain Chain, host, address string) (*Info, error) {
	v, err := getJSON(ctx, chain, strings.TrimSuffix(host, "/")+"/api/v2/smart-contracts/"+address)
	if err != nil {
		return nil, err
	}
	m := objOf(v)
	if m == nil {
		return nil, errors.New("接口返回的内容看不懂")
	}
	if pick(m, "abi") == nil && pickStr(m, "source_code") == "" && pickStr(m, "creation_bytecode") == "" {
		return nil, ErrNotFound
	}
	return parseBlockscout(m), nil
}

func parseBlockscout(m map[string]any) *Info {
	info := &Info{Verified: boolOf(pick(m, "is_verified"))}
	if v := pick(m, "abi"); v != nil {
		if b, err := json.Marshal(v); err == nil {
			info.ABI = b
		}
	}
	info.Name = pickStr(m, "name", "contract_name")
	info.Compiler = pickStr(m, "compiler_version")
	info.Language = pickStr(m, "language")
	info.License = pickStr(m, "license_type")
	info.CreationBytecode = hexCode(pickStr(m, "creation_bytecode"))
	if info.CreationBytecode != "" {
		info.creationIsOnchain = true
	}
	info.DeployedBytecode = hexCode(pickStr(m, "deployed_bytecode"))
	info.ConstructorArgs = hexCode(pickStr(m, "constructor_args"))
	info.CreationTxHash = pickStr(m, "creation_tx_hash", "creation_transaction_hash", "transaction_hash", "tx_hash")

	if src := pickStr(m, "source_code"); src != "" {
		if files := parseStandardJSONSources(src); len(files) > 0 {
			info.Sources = files
		} else {
			info.Sources = []SourceFile{{Path: mainFileName(info.Name), Content: src}}
		}
	}
	for _, raw := range pickArr(m, "additional_sources") {
		o := objOf(raw)
		if o == nil {
			continue
		}
		name := pickStr(o, "filename", "file_path", "path")
		content := pickStr(o, "source_code", "content")
		if name != "" && content != "" {
			info.Sources = append(info.Sources, SourceFile{Path: name, Content: content})
		}
	}
	for _, raw := range pickArr(m, "implementations") {
		o := objOf(raw)
		if o == nil {
			continue
		}
		if addr := pickStr(o, "address", "address_hash"); IsAddress(addr) {
			info.Implementations = append(info.Implementations, addr)
		}
	}
	if len(info.Implementations) > 0 || pickStr(m, "proxy_type") != "" {
		info.Proxy = true
	}
	return info
}

func fetchSourcify(ctx context.Context, chain Chain, address string) (*Info, error) {
	base := strings.TrimSuffix(chain.SourcifyBase, "/")
	if base == "" {
		base = "https://sourcify.dev/server"
	}
	url := fmt.Sprintf("%s/v2/contract/%d/%s?fields=abi,metadata,sources,compilation,creationBytecode,runtimeBytecode,deployment",
		base, chain.ChainID, address)
	v, err := getJSON(ctx, chain, url)
	if err != nil {
		return nil, err
	}
	m := objOf(v)
	if m == nil {
		return nil, errors.New("接口返回的内容看不懂")
	}
	info := parseSourcify(m)
	if info.ABI == nil && !info.HasCreationBytecode() && len(info.Sources) == 0 {
		return nil, ErrNotFound
	}
	return info, nil
}

func parseSourcify(m map[string]any) *Info {
	info := &Info{Verified: true}
	if v := pick(m, "abi"); v != nil {
		if b, err := json.Marshal(v); err == nil {
			info.ABI = b
		}
	}
	info.Compiler = pickStr(m, "compiler_version")
	if meta := pickObj(m, "metadata"); meta != nil {
		if c := pickObj(meta, "compiler"); c != nil {
			info.Compiler = pickStr(c, "version")
		}
		if info.ABI == nil {
			if out := pickObj(meta, "output"); out != nil {
				if v := pick(out, "abi"); v != nil {
					if b, err := json.Marshal(v); err == nil {
						info.ABI = b
					}
				}
			}
		}
		if s := pickObj(meta, "settings"); s != nil && info.Name == "" {
			// 形如 {"合约文件.sol": "合约名"}
			if target := pickObj(s, "compilationTarget"); target != nil {
				for _, v := range target {
					if name, ok := v.(string); ok && name != "" {
						info.Name = name
						break
					}
				}
			}
		}
		info.Sources = append(info.Sources, sourcesFromMap(pickObj(meta, "sources"))...)
	}
	if comp := pickObj(m, "compilation"); comp != nil {
		info.Compiler = pickStr(comp, "compilerVersion")
		info.Language = pickStr(comp, "language")
	}
	info.Sources = append(info.Sources, sourcesFromMap(pickObj(m, "sources"))...)

	if v := pick(m, "creationBytecode", "creation_bytecode"); v != nil {
		switch x := v.(type) {
		case string:
			info.CreationBytecode = hexCode(x)
		case map[string]any:
			// 「重新编译那份」是干净的；「链上那份」末尾往往粘着构造参数。
			if clean := hexCode(pickStr(x, "recompiledBytecode", "recompiled_bytecode", "bytecode")); clean != "" {
				info.CreationBytecode = clean
			} else if raw := hexCode(pickStr(x, "onchainBytecode", "onchain_bytecode")); raw != "" {
				info.CreationBytecode = raw
				info.creationIsOnchain = true
			}
		}
	}
	if v := pick(m, "deployedBytecode", "runtimeBytecode", "runtime_bytecode"); v != nil {
		switch x := v.(type) {
		case string:
			info.DeployedBytecode = hexCode(x)
		case map[string]any:
			info.DeployedBytecode = hexCode(pickStr(x, "recompiledBytecode", "bytecode", "onchainBytecode"))
		}
	}
	if dep := pickObj(m, "deployment"); dep != nil {
		info.ConstructorArgs = hexCode(pickStr(dep, "constructorArgs", "constructor_args", "constructorArguments"))
		info.CreationTxHash = pickStr(dep, "transactionHash", "transaction_hash")
	}
	if info.ConstructorArgs == "" {
		info.ConstructorArgs = hexCode(pickStr(m, "constructor_args", "constructorArgs"))
	}
	return info
}

func fetchEtherscan(ctx context.Context, chain Chain, address string) (*Info, error) {
	base := strings.TrimSuffix(chain.EtherscanBase, "/")
	if base == "" {
		base = "https://api.etherscan.io/v2/api"
	}
	url := fmt.Sprintf("%s?chainid=%d&module=contract&action=getsourcecode&address=%s&apikey=%s",
		base, chain.ChainID, address, chain.EtherscanAPIKey)
	v, err := getJSON(ctx, chain, url)
	if err != nil {
		return nil, err
	}
	m := objOf(v)
	if m == nil {
		return nil, errors.New("接口返回的内容看不懂")
	}
	if msg := pickStr(m, "message"); msg != "" && !strings.EqualFold(msg, "OK") {
		return nil, fmt.Errorf("区块浏览器接口说：%s", msg)
	}
	arr := pickArr(m, "result")
	if len(arr) == 0 {
		return nil, ErrNotFound
	}
	first := objOf(arr[0])
	if first == nil {
		return nil, ErrNotFound
	}
	if abi := pickStr(first, "ABI"); strings.Contains(abi, "not verified") {
		return nil, ErrNotFound
	}
	if pickStr(first, "SourceCode") == "" && pickStr(first, "ABI") == "" {
		return nil, ErrNotFound
	}
	return parseEtherscan(first), nil
}

func parseEtherscan(m map[string]any) *Info {
	info := &Info{Verified: true}
	info.Name = pickStr(m, "ContractName")
	info.Compiler = pickStr(m, "CompilerVersion")
	info.License = pickStr(m, "LicenseType")
	info.ConstructorArgs = hexCode(pickStr(m, "ConstructorArguments"))
	if abi := pickStr(m, "ABI"); abi != "" {
		info.ABI = json.RawMessage(abi)
	}
	if src := pickStr(m, "SourceCode"); src != "" {
		if files := parseStandardJSONSources(src); len(files) > 0 {
			info.Sources = files
		} else {
			info.Sources = []SourceFile{{Path: mainFileName(info.Name), Content: src}}
		}
	}
	if impl := pickStr(m, "Implementation"); IsAddress(impl) {
		info.Implementations = append(info.Implementations, impl)
	}
	if strings.EqualFold(pickStr(m, "Proxy"), "1") {
		info.Proxy = true
	}
	return info
}

// fetchCreationInputFromRPC 直接问链上节点：那笔创建合约的交易原始数据是什么。
// 这样即使合约没开源，也能拿到它的创建代码。
func fetchCreationInputFromRPC(ctx context.Context, chain Chain, txHash string) (string, error) {
	if !strings.HasPrefix(txHash, "0x") {
		return "", fmt.Errorf("创建交易号 %q 不合法", txHash)
	}
	var lastErr error
	for _, rpcURL := range chain.RPC {
		reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		v, err := postJSON(reqCtx, chain, rpcURL, map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"method": "eth_getTransactionByHash", "params": []any{txHash},
		})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		res := objOf(pick(objOf(v), "result"))
		if res == nil {
			lastErr = errors.New("节点里没有这笔交易")
			continue
		}
		if to := pickStr(res, "to"); to != "" && !strings.EqualFold(to, "0x0000000000000000000000000000000000000000") {
			lastErr = errors.New("这笔交易不是创建合约的交易")
			continue
		}
		if input := hexCode(pickStr(res, "input")); input != "" {
			return input, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的链上节点")
	}
	return "", lastErr
}

// ---------------------------------------------------------------- HTTP 与取值

func getJSON(ctx context.Context, chain Chain, url string) (any, error) {
	return doJSON(ctx, chain, http.MethodGet, url, nil)
}

func postJSON(ctx context.Context, chain Chain, url string, payload any) (any, error) {
	return doJSON(ctx, chain, http.MethodPost, url, payload)
}

func doJSON(ctx context.Context, chain Chain, method, url string, payload any) (any, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "msx-go/contractsource")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := chain.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("接口返回 %d", resp.StatusCode)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("接口返回了空内容")
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("接口返回的内容不是合法 JSON")
	}
	return v, nil
}

func objOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func pick(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func pickStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch x := m[k].(type) {
		case string:
			if strings.TrimSpace(x) != "" {
				return strings.TrimSpace(x)
			}
		case json.Number:
			return x.String()
		}
	}
	return ""
}

func pickObj(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if o := objOf(m[k]); o != nil {
			return o
		}
	}
	return nil
}

func pickArr(m map[string]any, keys ...string) []any {
	for _, k := range keys {
		if a, ok := m[k].([]any); ok && len(a) > 0 {
			return a
		}
	}
	return nil
}

func boolOf(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	case json.Number:
		return x.String() != "0"
	}
	return false
}

// hexCode 收拾成 0x 开头的小写十六进制；内容不合法时返回空串。
func hexCode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if s == "" || len(s)%2 != 0 {
		return ""
	}
	if _, err := hex.DecodeString(s); err != nil {
		return ""
	}
	return "0x" + strings.ToLower(s)
}

func mainFileName(contractName string) string {
	if contractName == "" {
		return "Contract.sol"
	}
	return contractName + ".sol"
}

func sourcesFromMap(m map[string]any) []SourceFile {
	if m == nil {
		return nil
	}
	out := make([]SourceFile, 0, len(m))
	for path, raw := range m {
		switch x := raw.(type) {
		case string:
			out = append(out, SourceFile{Path: path, Content: x})
		case map[string]any:
			if content := pickStr(x, "content", "source_code"); content != "" {
				out = append(out, SourceFile{Path: path, Content: content})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out
}

// parseStandardJSONSources 认出手写「标准 JSON 输入」格式的源码（多文件合约常见）。
func parseStandardJSONSources(raw string) []SourceFile {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	if strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") {
		s = strings.TrimSpace(s[1 : len(s)-1]) // 区块浏览器会多包一层大括号
	}
	var std struct {
		Sources map[string]struct {
			Content string `json:"content"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(s), &std); err != nil || len(std.Sources) == 0 {
		return nil
	}
	out := make([]SourceFile, 0, len(std.Sources))
	for path, f := range std.Sources {
		out = append(out, SourceFile{Path: path, Content: f.Content})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out
}
