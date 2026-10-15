package evmabi

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

func TestParseTypeAndString(t *testing.T) {
	cases := []string{
		"uint256", "uint8", "uint", "int256", "address", "bool",
		"bytes", "bytes32", "bytes1", "string",
		"address[]", "uint256[]", "bytes[]", "address[3]",
		"(address,address,uint256,uint24,uint160)",
		"(bytes,address,uint256,uint256,uint256)",
		"((address,uint256),bytes)[]",
	}
	for _, in := range cases {
		ty, err := ParseType(in)
		if err != nil {
			t.Fatalf("解析 %q 出错: %v", in, err)
		}
		got := ty.String()
		want := in
		if in == "uint" {
			want = "uint256" // 规范里 uint 就是 uint256
		}
		if got != want {
			t.Errorf("ParseType(%q).String() = %q, 期望 %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "uint7", "bytes33", "address[", "(", "address)", "foo"} {
		if _, err := ParseType(bad); err == nil {
			t.Errorf("ParseType(%q) 本应报错却通过了", bad)
		}
	}
}

func TestParseSignature(t *testing.T) {
	name, types, err := ParseSignature("swapExactTokensForTokens(uint256,uint256,address[],address,uint256)")
	if err != nil {
		t.Fatal(err)
	}
	if name != "swapExactTokensForTokens" || len(types) != 5 {
		t.Fatalf("解析结果不对: %s %d 个参数", name, len(types))
	}
	// 元组里的逗号不能被当成顶层分隔符
	name, types, err = ParseSignature("exactInputSingle((address,address,uint24,address,uint256,uint256,uint256,uint160))")
	if err != nil {
		t.Fatal(err)
	}
	if name != "exactInputSingle" || len(types) != 1 || types[0].kind != kindTuple || len(types[0].field) != 8 {
		t.Fatalf("元组签名解析结果不对: %s %v", name, types)
	}
	if _, _, err := ParseSignature("balanceOf(address"); err == nil {
		t.Error("缺少右括号本应报错")
	}
}

// 用公开可查的 ERC20 transfer calldata 钉住编码结果。
func TestEncodeTransferExactBytes(t *testing.T) {
	call, err := NewCall("transfer(address,uint256)",
		Address("0x5B38Da6a701c568545dCfcB03FcB875f56beddC4"), Uint(1000))
	if err != nil {
		t.Fatal(err)
	}
	data, err := call.Data()
	if err != nil {
		t.Fatal(err)
	}
	want := "a9059cbb" +
		"0000000000000000000000005b38da6a701c568545dcfcb03fcb875f56beddc4" +
		"00000000000000000000000000000000000000000000000000000000000003e8"
	if hex.EncodeToString(data) != want {
		t.Errorf("transfer calldata = %s\n期望 %s", hex.EncodeToString(data), want)
	}
}

// 动态数组参数（V2 的 path）的偏移量写法要和规范一致。
func TestEncodeGetAmountsOutExactBytes(t *testing.T) {
	call, err := NewCall("getAmountsOut(uint256,address[])",
		BigUint(new(big.Int).SetUint64(1e18)),
		ArrayVal(
			Address("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
			Address("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		))
	if err != nil {
		t.Fatal(err)
	}
	data, err := call.Data()
	if err != nil {
		t.Fatal(err)
	}
	want := "d06ca61f" +
		"0000000000000000000000000000000000000000000000000de0b6b3a7640000" +
		"0000000000000000000000000000000000000000000000000000000000000040" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"000000000000000000000000c02aaa39b223fe8d0a0e5c4f27ead9083c756cc2" +
		"000000000000000000000000a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	if hex.EncodeToString(data) != want {
		t.Errorf("getAmountsOut calldata = %s\n期望 %s", hex.EncodeToString(data), want)
	}
}

// V3 的 exactInput 参数里带 bytes path（元组里套动态类型），编码后要能原样解回来。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	path := append(append(
		mustAddr(t, "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		[]byte{0x00, 0x0b, 0xb8}...), // fee = 3000
		mustAddr(t, "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")...)

	cases := []struct {
		sig  string
		args []Value
	}{
		{"balanceOf(address)", []Value{Address("0x0000000000000000000000000000000000000001")}},
		{"decimals()", nil},
		{"quoteExactInputSingle((address,address,uint256,uint24,uint160))", []Value{TupleVal(
			Address("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
			Address("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			BigUint(new(big.Int).SetUint64(1e18)),
			Uint(3000),
			BigUint(big.NewInt(0)),
		)}},
		{"exactInput((bytes,address,uint256,uint256,uint256))", []Value{TupleVal(
			BytesVal(path),
			Address("0x1111111111111111111111111111111111111111"),
			BigUint(new(big.Int).SetUint64(1e18)),
			BigUint(big.NewInt(123456)),
			BigUint(big.NewInt(1700000000)),
		)}},
		{"multicall(uint256,bytes[])", []Value{
			BigUint(big.NewInt(1700000000)),
			ArrayVal(BytesVal([]byte{1, 2, 3}), BytesVal([]byte{4, 5})),
		}},
		{"swapExactTokensForTokens(uint256,uint256,address[],address,uint256)", []Value{
			BigUint(new(big.Int).SetUint64(1e18)),
			BigUint(big.NewInt(999)),
			ArrayVal(Address("0x0000000000000000000000000000000000000002"),
				Address("0x0000000000000000000000000000000000000003"),
				Address("0x0000000000000000000000000000000000000004")),
			Address("0x0000000000000000000000000000000000000005"),
			BigUint(big.NewInt(1700000000)),
		}},
	}
	for _, c := range cases {
		call, err := NewCall(c.sig, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", c.sig, err)
		}
		data, err := call.Data()
		if err != nil {
			t.Fatalf("%s: %v", c.sig, err)
		}
		if len(data)%32 != 4 {
			t.Errorf("%s 编码长度 %d 不是 4 + 32 的整数倍", c.sig, len(data))
		}
		back, err := Decode(call.types, data[4:])
		if err != nil {
			t.Fatalf("%s 解回来出错: %v", c.sig, err)
		}
		for i := range back {
			if !equalValue(c.args[i], back[i]) {
				t.Errorf("%s 第 %d 个参数往返后不一致：%+v → %+v", c.sig, i+1, c.args[i], back[i])
			}
		}
	}
}

func equalValue(a, b Value) bool {
	switch {
	case a.Uint != nil || b.Uint != nil:
		return a.Int().Cmp(b.Int()) == 0
	case a.Addr != "" || b.Addr != "":
		return strings.EqualFold(a.Addr, b.Addr)
	case len(a.Array) > 0 || len(b.Array) > 0:
		if len(a.Array) != len(b.Array) {
			return false
		}
		for i := range a.Array {
			if !equalValue(a.Array[i], b.Array[i]) {
				return false
			}
		}
		return true
	case len(a.Tuple) > 0 || len(b.Tuple) > 0:
		if len(a.Tuple) != len(b.Tuple) {
			return false
		}
		for i := range a.Tuple {
			if !equalValue(a.Tuple[i], b.Tuple[i]) {
				return false
			}
		}
		return true
	default:
		return string(a.Bytes) == string(b.Bytes)
	}
}

// 真实合约返回值：getAmountsOut 回 uint256[]，QuoterV2 回四个值。
func TestDecodeRealWorldReturns(t *testing.T) {
	amounts := "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000003" +
		"0000000000000000000000000000000000000000000000000de0b6b3a7640000" +
		"00000000000000000000000000000000000000000000000000000000000f4240" +
		"0000000000000000000000000000000000000000000000000000000005f5e100"
	raw, _ := hex.DecodeString(strings.TrimPrefix(amounts, "0x"))
	ty := MustParseType("uint256[]")
	out, err := Decode([]Type{ty}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || len(out[0].Array) != 3 {
		t.Fatalf("解出来的不是 3 个数的数组: %+v", out)
	}
	if out[0].Array[2].Int().String() != "100000000" {
		t.Errorf("最后一个数 = %s, 期望 100000000", out[0].Array[2].Int())
	}

	// QuoterV2.quoteExactInputSingle 的返回值
	q := "0x" +
		"0000000000000000000000000000000000000000000000000000000000007b0c" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"0000000000000000000000000000000000000000000000000000000000019a28"
	rawQ, _ := hex.DecodeString(strings.TrimPrefix(q, "0x"))
	types := mustParseList(t, "uint256,uint160,uint32,uint256")
	vals, err := Decode(types, rawQ)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0].Int().Int64() != 31500 || vals[2].Int().Int64() != 2 || vals[3].Int().Int64() != 105000 {
		t.Errorf("QuoterV2 返回值解错了: %v %v %v %v", vals[0].Int(), vals[1].Int(), vals[2].Int(), vals[3].Int())
	}

	// symbol() 这类字符串返回值：偏移 + 长度 + 内容
	sym := "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000004" +
		"5553444300000000000000000000000000000000000000000000000000000000"
	rawSym, _ := hex.DecodeString(strings.TrimPrefix(sym, "0x"))
	sv, err := Decode([]Type{MustParseType("string")}, rawSym)
	if err != nil {
		t.Fatal(err)
	}
	if sv[0].Text() != "USDC" {
		t.Errorf("symbol 解出来 = %q, 期望 USDC", sv[0].Text())
	}

	// 只有一行的 uint256 返回值
	one := make([]byte, 32)
	one[31] = 6
	dv, err := Decode([]Type{MustParseType("uint8")}, one)
	if err != nil || dv[0].Int().Int64() != 6 {
		t.Errorf("decimals 解出来 = %v (%v), 期望 6", dv, err)
	}
}

func TestEncodeRejectsBadInput(t *testing.T) {
	if _, err := NewCall("transfer(address,uint256)", Address("0x123")); err == nil {
		t.Error("地址长度不对本应报错")
	}
	call, _ := NewCall("transfer(address,uint256)", Address("0x1111111111111111111111111111111111111111"), Uint(1))
	if _, err := call.Data(); err != nil {
		t.Errorf("正常调用不该报错: %v", err)
	}
	bad := Call{signature: "transfer(address,uint256)", types: []Type{MustParseType("address")}}
	if _, err := bad.Data(); err == nil {
		t.Error("类型与值个数不匹配本应报错")
	}
	if _, err := NewCall("transfer(address,uint256)", Address("0x1111111111111111111111111111111111111111")); err == nil {
		t.Error("参数少了一个本应报错")
	}
	if _, err := Decode([]Type{MustParseType("uint256[]")}, []byte{1, 2, 3}); err == nil {
		t.Error("数据太短本应报错")
	}
	if _, err := NormalizeAddress("0xZZ11111111111111111111111111111111111111"); err == nil {
		t.Error("非十六进制地址本应报错")
	}
	// 大写地址能正常规范化
	got, err := NormalizeAddress("0x5B38DA6A701C568545DCFCB03FCB875F56BEDDC4")
	if err != nil || got != "0x5b38da6a701c568545dcfcb03fcb875f56beddc4" {
		t.Errorf("地址规范化结果 = %q (%v)", got, err)
	}
}

// 负数（int256）要用补码表示。
func TestEncodeNegativeInt(t *testing.T) {
	call, err := NewCall("f(int256)", BigUint(big.NewInt(-1)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := call.Data()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(data[4:]) != strings.Repeat("f", 64) {
		t.Errorf("-1 的 int256 编码 = %s, 期望全 f", hex.EncodeToString(data[4:]))
	}
	if _, err := NewCall("f(uint256)", BigUint(big.NewInt(-1))); err != nil {
		t.Fatal(err)
	}
	c2, _ := NewCall("f(uint256)", BigUint(big.NewInt(-1)))
	if _, err := c2.Data(); err == nil {
		t.Error("无符号参数传负数本应报错")
	}
}

func mustAddr(t *testing.T, s string) []byte {
	t.Helper()
	b, err := AddressBytes(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustParseList(t *testing.T, s string) []Type {
	t.Helper()
	types, err := ParseTypeList(s)
	if err != nil {
		t.Fatal(err)
	}
	return types
}
