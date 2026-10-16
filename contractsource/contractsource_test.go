package contractsource

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rubik-crypto/msx-go/evmabi"
)

func typesOf(t *testing.T, list ...string) []evmabi.Type {
	t.Helper()
	out := make([]evmabi.Type, len(list))
	for i, s := range list {
		tt, err := evmabi.ParseType(s)
		if err != nil {
			t.Fatalf("解析类型 %s 失败：%v", s, err)
		}
		out[i] = tt
	}
	return out
}

func TestSplitConstructorArgs(t *testing.T) {
	code := []byte{0x60, 0x80, 0x60, 0x40, 0x52}
	types := typesOf(t, "string", "uint256", "address[]")
	values := []evmabi.Value{
		evmabi.StringVal("我的代币"),
		evmabi.Uint(1000000),
		evmabi.ArrayVal(evmabi.Address("0x1111111111111111111111111111111111111111")),
	}
	args, err := evmabi.Encode(types, values)
	if err != nil {
		t.Fatal(err)
	}
	full := append(append([]byte{}, code...), args...)

	gotCode, gotArgs, ok := SplitConstructorArgs(full, types)
	if !ok {
		t.Fatal("应该能切出构造参数")
	}
	if hex.EncodeToString(gotCode) != hex.EncodeToString(code) {
		t.Fatalf("切出来的部署代码不对：%x", gotCode)
	}
	if hex.EncodeToString(gotArgs) != hex.EncodeToString(args) {
		t.Fatalf("切出来的参数不对：%x", gotArgs)
	}

	// 没有参数类型时不许乱切
	if _, _, ok := SplitConstructorArgs(full, nil); ok {
		t.Fatal("没有参数类型时不该切任何东西")
	}
	// 定长参数：末尾补一段凑出来的数据，不能认成参数
	one := typesOf(t, "uint256")
	oneArgs, _ := evmabi.Encode(one, []evmabi.Value{evmabi.Uint(7)})
	if _, _, ok := SplitConstructorArgs(append(append([]byte{}, code...), oneArgs...), typesOf(t, "uint256", "uint256", "uint256")); ok {
		t.Fatal("长度不够的参数不该被认出来")
	}
}

func TestBuildDeployData(t *testing.T) {
	types := typesOf(t, "string", "uint8")
	values := []evmabi.Value{evmabi.StringVal("MYT"), evmabi.Uint(18)}
	data, err := BuildDeployData("0x6080", types, values, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(data, "0x6080") {
		t.Fatalf("生成的数据应该以创建代码开头，实际 %s", data[:10])
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	args, err := evmabi.Encode(types, values)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2+len(args) {
		t.Fatalf("总长度应该是 2 + %d，实际 %d", len(args), len(raw))
	}

	// 库占位符：没填要报错，填了要替换进去
	codeWithLib := "0x6080__$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa$__6040"
	if _, err := BuildDeployData(codeWithLib, nil, nil, nil); err == nil {
		t.Fatal("库地址没填时应该报错")
	}
	libs := map[string]string{"__$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa$__": "0x2222222222222222222222222222222222222222"}
	out, err := BuildDeployData(codeWithLib, nil, nil, libs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, strings.Repeat("2", 40)) {
		t.Fatalf("库地址没有被替换进去：%s", out)
	}
}

func TestFetchBlockscoutStripsConstructorArgs(t *testing.T) {
	types := typesOf(t, "string")
	args, err := evmabi.Encode(types, []evmabi.Value{evmabi.StringVal("Tether USD")})
	if err != nil {
		t.Fatal(err)
	}
	code := "0x6080604052"
	full := code + hex.EncodeToString(args)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v2/smart-contracts/") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{
			"is_verified": true,
			"name": "Token",
			"compiler_version": "v0.8.20",
			"language": "Solidity",
			"abi": [{"type":"constructor","inputs":[{"name":"_name","type":"string"}]}],
			"source_code": "contract Token {}",
			"creation_bytecode": %q,
			"deployed_bytecode": "0x6000",
			"constructor_args": %q
		}`, full, "0x"+hex.EncodeToString(args))
	}))
	defer srv.Close()

	info, err := Fetch(context.Background(), Chain{ChainID: 1, BlockscoutHost: srv.URL},
		"0xdAC17F958D2ee523a2206206994597C13D831ec7")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "Token" || !info.Verified {
		t.Fatalf("基本字段没读对：%+v", info)
	}
	if info.CreationBytecode != code {
		t.Fatalf("末尾的构造参数应该被切掉，现在是 %s", info.CreationBytecode)
	}
	if info.ConstructorArgs != "0x"+hex.EncodeToString(args) {
		t.Fatalf("构造参数没读对：%s", info.ConstructorArgs)
	}
	if len(info.Sources) != 1 || info.Sources[0].Content != "contract Token {}" {
		t.Fatalf("源码没读对：%+v", info.Sources)
	}
}

func TestFetchSourcifyPrefersRecompiled(t *testing.T) {
	types := typesOf(t, "uint256")
	args, err := evmabi.Encode(types, []evmabi.Value{evmabi.Uint(42)})
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"abi": []any{map[string]any{"type": "constructor", "inputs": []any{
			map[string]any{"name": "supply", "type": "uint256"},
		}}},
		"metadata": map[string]any{
			"settings": map[string]any{"compilationTarget": map[string]any{"MyToken.sol": "MyToken"}},
			"compiler": map[string]any{"version": "0.8.20+commit"},
		},
		"sources": map[string]any{"MyToken.sol": map[string]any{"content": "contract MyToken {}"}},
		"creationBytecode": map[string]any{
			"recompiledBytecode": "0x6080604052",
			"onchainBytecode":    "0x6080604052" + hex.EncodeToString(args),
		},
		"runtimeBytecode": map[string]any{"recompiledBytecode": "0x6000"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	info, err := Fetch(context.Background(), Chain{ChainID: 56, SourcifyBase: srv.URL},
		"0x55d398326f99059fF775485246999027B3197955")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "MyToken" {
		t.Fatalf("合约名应该从 metadata 里取到，实际 %q", info.Name)
	}
	if info.CreationBytecode != "0x6080604052" {
		t.Fatalf("应该优先用重新编译的那份干净代码，实际 %s", info.CreationBytecode)
	}
	if info.DeployedBytecode != "0x6000" || info.Compiler != "0.8.20+commit" {
		t.Fatalf("运行代码或编译器版本没读对：%s / %s", info.DeployedBytecode, info.Compiler)
	}
	if len(info.Sources) != 1 || info.Sources[0].Path != "MyToken.sol" {
		t.Fatalf("源码没读对：%+v", info.Sources)
	}
}

func TestFetchSourcifyStripsOnchainArgs(t *testing.T) {
	types := typesOf(t, "string")
	args, err := evmabi.Encode(types, []evmabi.Value{evmabi.StringVal("dave")})
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"abi": []any{map[string]any{"type": "constructor", "inputs": []any{
			map[string]any{"name": "who", "type": "string"},
		}}},
		"creationBytecode": map[string]any{"onchainBytecode": "0x6080" + hex.EncodeToString(args)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	info, err := Fetch(context.Background(), Chain{ChainID: 1, SourcifyBase: srv.URL},
		"0xdAC17F958D2ee523a2206206994597C13D831ec7")
	if err != nil {
		t.Fatal(err)
	}
	if info.CreationBytecode != "0x6080" {
		t.Fatalf("只有链上那份时应该把末尾参数切掉，实际 %s", info.CreationBytecode)
	}
	if info.ConstructorArgs != "0x"+hex.EncodeToString(args) {
		t.Fatalf("切下来的参数不对：%s", info.ConstructorArgs)
	}
}

func TestFetchFallsBackToChainTransaction(t *testing.T) {
	var rpcCalled bool
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpcCalled = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"to": nil, "input": "0x6080604052"},
		})
	}))
	defer rpc.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"is_verified":      false,
			"creation_tx_hash": "0xabcdef",
			"abi":              []any{},
		})
	}))
	defer srv.Close()

	// 源码仓库也指到一个「什么都没有」的服务上，保证这次测试不出网
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer empty.Close()

	info, err := Fetch(context.Background(),
		Chain{ChainID: 1, BlockscoutHost: srv.URL, SourcifyBase: empty.URL, RPC: []string{rpc.URL}},
		"0xdAC17F958D2ee523a2206206994597C13D831ec7")
	if err != nil {
		t.Fatal(err)
	}
	if !rpcCalled {
		t.Fatal("没有创建代码时应该去链上取那笔创建交易的原始数据")
	}
	if info.CreationBytecode != "0x6080604052" {
		t.Fatalf("链上取回来的创建代码不对：%s", info.CreationBytecode)
	}
	if !info.CreationBytecodeFromChain() {
		t.Fatal("从链上交易取回来的代码应该标明来源")
	}
}

func TestFetchRejectsBadAddress(t *testing.T) {
	if _, err := Fetch(context.Background(), Chain{ChainID: 1}, "0x123"); err == nil {
		t.Fatal("地址不合法时应该直接报错")
	}
}

func TestParseStandardJSONSources(t *testing.T) {
	raw := `{{"language":"Solidity","sources":{"A.sol":{"content":"contract A {}"},"B.sol":{"content":"contract B {}"}}}}`
	files := parseStandardJSONSources(raw)
	if len(files) != 2 || files[0].Path != "A.sol" {
		t.Fatalf("标准 JSON 源码没解析对：%+v", files)
	}
	if files := parseStandardJSONSources("contract A {}"); files != nil {
		t.Fatalf("普通源码不该被当成标准 JSON：%+v", files)
	}
}
