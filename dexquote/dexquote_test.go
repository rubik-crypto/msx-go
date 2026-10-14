package dexquote

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/rubik-crypto/msx-go/evmabi"
)

const (
	weth = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
	usdc = "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
	dai  = "0x6B175474E89094C44Da98b954EedeAC495271d0F"
	user = "0x5B38Da6a701c568545dCfcB03FcB875f56beddC4"
)

func v2Dex() Dex {
	return Dex{ID: "uni-v2", Name: "Uniswap V2", Kind: KindV2, Router: "0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D", Wrapped: weth}
}

func v3Dex(style string) Dex {
	return Dex{
		ID: "uni-v3", Name: "Uniswap V3", Kind: KindV3,
		Router: "0xE592427A0AEce92De3Edee1F18E0157C05861564",
		Quoter: "0x61fFE014bA17989E743c5F6cB21bF9697530B21e",
		Fees:   []uint32{500, 3000, 10000}, RouterStyle: style, QuoterStyle: QuoterStyleV2, Wrapped: weth,
	}
}

// decodeCall 把 calldata 按签名解回参数，用来说明「发出去的这笔交易到底写了什么」。
func decodeCall(t *testing.T, signature string, data []byte) []evmabi.Value {
	t.Helper()
	name, types, err := evmabi.ParseSignature(signature)
	if err != nil {
		t.Fatalf("签名 %q 有问题: %v", name, err)
	}
	want := evmabi.Hex(evmabi.Selector(signature))
	if got := evmabi.Hex(data[:4]); got != want {
		t.Fatalf("选择器不是 %s，实际 %s（签名 %s）", want, got, signature)
	}
	vals, err := evmabi.Decode(types, data[4:])
	if err != nil {
		t.Fatalf("按 %s 解 calldata 失败: %v", signature, err)
	}
	return vals
}

func uintVal(t *testing.T, data []byte, signature string, i int) *big.Int {
	t.Helper()
	return decodeCall(t, signature, data)[i].Int()
}

func TestQuoteAllV2(t *testing.T) {
	var got [][]Call
	caller := func(ctx context.Context, calls []Call) ([]Result, error) {
		got = append(got, calls)
		// 回一个 getAmountsOut 的结果：amounts = [in, out]
		return []Result{{Data: abiUintArray([]uint64{1e18, 2500000000})}}, nil
	}
	q := New(caller)
	hops := []Hop{{From: usdc, To: weth}, {From: weth, To: dai}}
	quotes, err := q.QuoteAll(context.Background(), []Query{{
		Dex: v2Dex(), Hops: hops, AmountIn: new(big.Int).SetUint64(1e18),
	}}, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes) != 1 || quotes[0].Err != nil {
		t.Fatalf("报价结果不对: %+v", quotes)
	}
	if quotes[0].AmountOut.String() != "2500000000" {
		t.Errorf("换出量 = %s，期望 2500000000", quotes[0].AmountOut)
	}
	// 发出的调用应该问在路由合约上，calldata 是 getAmountsOut
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("应该只发一批、一批一条，实际 %+v", got)
	}
	if got[0][0].To != v2Dex().Router {
		t.Errorf("调用发给了 %s，期望路由合约", got[0][0].To)
	}
	vals := decodeCall(t, "getAmountsOut(uint256,address[])", got[0][0].Data)
	if vals[0].Int().Uint64() != 1e18 {
		t.Errorf("问的数量 = %s", vals[0].Int())
	}
	path := vals[1].Array
	if len(path) != 3 || !strings.EqualFold(path[0].Addr, usdc) || !strings.EqualFold(path[2].Addr, dai) {
		t.Errorf("路线不对: %+v", path)
	}
}

func TestQuoteAllV3SingleAndMulti(t *testing.T) {
	var calls []Call
	caller := func(ctx context.Context, cs []Call) ([]Result, error) {
		calls = cs
		out := make([]Result, len(cs))
		for i := range out {
			out[i] = Result{Data: abiUint4(uint64(5000000+i), 0, 3, 120000)}
		}
		return out, nil
	}
	q := New(caller)
	dex := v3Dex(RouterStyleV1)
	queries := []Query{
		{Dex: dex, Hops: []Hop{{From: usdc, To: weth, Fee: 500}}, AmountIn: big.NewInt(1000)},
		{Dex: dex, Hops: []Hop{{From: usdc, To: weth, Fee: 3000}, {From: weth, To: dai, Fee: 3000}}, AmountIn: big.NewInt(1000)},
	}
	quotes, err := q.QuoteAll(context.Background(), queries, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("应该一次问两条路线，实际 %d 条", len(calls))
	}
	if quotes[0].AmountOut.String() != "5000000" || quotes[1].AmountOut.String() != "5000001" {
		t.Errorf("报价结果不对: %v / %v", quotes[0].AmountOut, quotes[1].AmountOut)
	}
	for _, c := range calls {
		if c.To != dex.Quoter {
			t.Errorf("报价应该问报价合约，实际问的是 %s", c.To)
		}
	}
	// 单跳：元组形式的 quoteExactInputSingle
	vals := decodeCall(t, "quoteExactInputSingle((address,address,uint256,uint24,uint160))", calls[0].Data)
	tuple := vals[0].Tuple
	if !strings.EqualFold(tuple[0].Addr, usdc) || !strings.EqualFold(tuple[1].Addr, weth) || tuple[3].Int().Uint64() != 500 {
		t.Errorf("单跳参数不对: %+v", tuple)
	}
	// 多跳：bytes path，格式是 地址(20)+手续费(3)+地址…
	vals = decodeCall(t, "quoteExactInput(bytes,uint256)", calls[1].Data)
	packed := vals[0].Bytes
	if len(packed) != 20+3+20+3+20 {
		t.Fatalf("多跳路径长度 = %d，期望 66", len(packed))
	}
	if !strings.EqualFold("0x"+hex.EncodeToString(packed[:20]), usdc) {
		t.Errorf("路径开头不是 usdc: %x", packed[:20])
	}
	fee := uint32(packed[20])<<16 | uint32(packed[21])<<8 | uint32(packed[22])
	if fee != 3000 {
		t.Errorf("第一跳手续费 = %d，期望 3000", fee)
	}
}

// Quoter 一代只回一个数，也要能解出来（有些链上装的是老合约）。
func TestQuoteV3QuoterStyleV1(t *testing.T) {
	caller := func(ctx context.Context, calls []Call) ([]Result, error) {
		return []Result{{Data: abiUint(777)}}, nil
	}
	dex := v3Dex(RouterStyleV1)
	dex.QuoterStyle = QuoterStyleV1
	quotes, err := New(caller).QuoteAll(context.Background(), []Query{{
		Dex: dex, Hops: []Hop{{From: usdc, To: weth, Fee: 500}}, AmountIn: big.NewInt(1000),
	}}, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if quotes[0].AmountOut == nil || quotes[0].AmountOut.Uint64() != 777 {
		t.Fatalf("老版报价合约的返回值没解出来: %+v", quotes[0])
	}
}

func TestNoPoolIsIsolated(t *testing.T) {
	caller := func(ctx context.Context, calls []Call) ([]Result, error) {
		out := make([]Result, len(calls))
		for i := range out {
			if i == 0 {
				out[i] = Result{Err: errors.New("execution reverted: INSUFFICIENT_LIQUIDITY")}
			} else {
				out[i] = Result{Data: abiUintArray([]uint64{10, 42})}
			}
		}
		return out, nil
	}
	quotes, err := New(caller).QuoteAll(context.Background(), []Query{
		{Dex: v2Dex(), Hops: []Hop{{From: usdc, To: weth}}, AmountIn: big.NewInt(10)},
		{Dex: v2Dex(), Hops: []Hop{{From: usdc, To: dai}}, AmountIn: big.NewInt(10)},
	}, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if quotes[0].Err == nil {
		t.Error("第一条应该带着错误")
	}
	if quotes[1].Err != nil || quotes[1].AmountOut.Uint64() != 42 {
		t.Errorf("一条失败不该影响另一条: %+v", quotes[1])
	}
}

func TestQuoteZeroResultIsNoPool(t *testing.T) {
	caller := func(ctx context.Context, calls []Call) ([]Result, error) {
		out := make([]Result, len(calls))
		for i := range out {
			out[i] = Result{Data: abiUintArray([]uint64{1000, 0})}
		}
		return out, nil
	}
	quotes, _ := New(caller).QuoteAll(context.Background(), []Query{{
		Dex: v2Dex(), Hops: []Hop{{From: usdc, To: weth}}, AmountIn: big.NewInt(10),
	}}, "latest")
	if quotes[0].Err == nil || quotes[0].AmountOut != nil {
		t.Errorf("换出量为 0 应该按「没有池子」处理: %+v", quotes[0])
	}
}

func TestQuoteRejectsBadInput(t *testing.T) {
	q := New(func(ctx context.Context, calls []Call) ([]Result, error) { return nil, nil })
	quotes, err := q.QuoteAll(context.Background(), []Query{
		{Dex: v2Dex(), Hops: []Hop{{From: usdc, To: weth}}, AmountIn: big.NewInt(0)},
		{Dex: Dex{ID: "x", Kind: "v9"}, Hops: []Hop{{From: usdc, To: weth}}, AmountIn: big.NewInt(1)},
		{Dex: v2Dex(), Hops: []Hop{{From: usdc}}, AmountIn: big.NewInt(1)},
	}, "latest")
	if err != nil {
		t.Fatal(err)
	}
	for i, qt := range quotes {
		if qt.Err == nil {
			t.Errorf("第 %d 条本来就不该报出价来: %+v", i+1, qt)
		}
	}
}

func TestPackPath(t *testing.T) {
	packed, err := PackPath([]Hop{{From: usdc, To: weth, Fee: 3000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(packed) != 43 {
		t.Fatalf("单跳路径应该是 43 字节，实际 %d", len(packed))
	}
	if _, err := PackPath([]Hop{{From: usdc, To: weth, Fee: 1 << 25}}); err == nil {
		t.Error("手续费超出 3 字节应该报错")
	}
	if _, err := PackPath(nil); err == nil {
		t.Error("空路线应该报错")
	}
	if _, err := PackPath([]Hop{{From: "0x123", To: weth}}); err == nil {
		t.Error("地址不合法应该报错")
	}
}

func TestSwapCalldataV2(t *testing.T) {
	dex := v2Dex()
	amountIn := big.NewInt(1000000)
	minOut := big.NewInt(990000)
	const deadline = 1700000000

	// 代币换代币
	tx, err := SwapCalldata(dex, []Hop{{From: usdc, To: weth}, {From: weth, To: dai}}, amountIn, minOut, user, deadline, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if tx.To != dex.Router || tx.Value.Sign() != 0 {
		t.Errorf("收件地址/附带金额不对: %+v", tx)
	}
	vals := decodeCall(t, "swapExactTokensForTokens(uint256,uint256,address[],address,uint256)", tx.Data)
	if vals[0].Int().Cmp(amountIn) != 0 || vals[1].Int().Cmp(minOut) != 0 {
		t.Error("数量或最少到手量不对")
	}
	if len(vals[2].Array) != 3 || !strings.EqualFold(vals[3].Addr, user) || vals[4].Int().Uint64() != deadline {
		t.Error("路线 / 收货地址 / 过期时间不对")
	}

	// 用原生币买：走 payable 函数，value 等于卖出量
	tx, err = SwapCalldata(dex, []Hop{{From: NativePlaceholder, To: usdc}}, amountIn, minOut, user, deadline, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Value.Cmp(amountIn) != 0 {
		t.Errorf("原生币买入的 value = %s，期望 %s", tx.Value, amountIn)
	}
	vals = decodeCall(t, "swapExactETHForTokens(uint256,address[],address,uint256)", tx.Data)
	if len(vals[1].Array) != 2 || !strings.EqualFold(vals[1].Array[0].Addr, weth) {
		t.Error("原生币买入时路线第一跳应该走包装合约")
	}

	// 换成原生币
	tx, err = SwapCalldata(dex, []Hop{{From: usdc, To: NativePlaceholder}}, amountIn, minOut, user, deadline, false, true)
	if err != nil {
		t.Fatal(err)
	}
	vals = decodeCall(t, "swapExactTokensForETH(uint256,uint256,address[],address,uint256)", tx.Data)
	if !strings.EqualFold(vals[2].Array[1].Addr, weth) {
		t.Error("换成原生币时路线最后一跳应该是包装合约")
	}
	if tx.Value.Sign() != 0 {
		t.Error("代币换原生币不该附带原生币")
	}
}

func TestSwapCalldataV3Direct(t *testing.T) {
	amountIn := big.NewInt(1000000)
	minOut := big.NewInt(990000)
	const deadline = 1700000000
	hops := []Hop{{From: usdc, To: weth, Fee: 3000}}

	// 老 SwapRouter：参数里带 deadline
	tx, err := SwapCalldata(v3Dex(RouterStyleV1), hops, amountIn, minOut, user, deadline, false, false)
	if err != nil {
		t.Fatal(err)
	}
	vals := decodeCall(t, "exactInputSingle((address,address,uint24,address,uint256,uint256,uint256,uint160))", tx.Data)
	p := vals[0].Tuple
	if !strings.EqualFold(p[0].Addr, usdc) || !strings.EqualFold(p[1].Addr, weth) || p[2].Int().Uint64() != 3000 {
		t.Error("币对或手续费档位不对")
	}
	if !strings.EqualFold(p[3].Addr, user) {
		t.Error("收货地址应该是用户")
	}
	if p[4].Int().Uint64() != deadline || p[5].Int().Cmp(amountIn) != 0 || p[6].Int().Cmp(minOut) != 0 {
		t.Error("过期时间 / 卖出量 / 最少到手量不对")
	}

	// 新 SwapRouter02：参数里没有 deadline
	tx, err = SwapCalldata(v3Dex(RouterStyleV2), hops, amountIn, minOut, user, deadline, false, false)
	if err != nil {
		t.Fatal(err)
	}
	vals = decodeCall(t, "exactInputSingle((address,address,uint24,address,uint256,uint256,uint160))", tx.Data)
	if len(vals[0].Tuple) != 7 {
		t.Errorf("SwapRouter02 的参数应该是 7 个字段，实际 %d", len(vals[0].Tuple))
	}
}

func TestSwapCalldataV3MultiHop(t *testing.T) {
	amountIn := big.NewInt(1000000)
	minOut := big.NewInt(990000)
	const deadline = 1700000000
	hops := []Hop{{From: usdc, To: weth, Fee: 500}, {From: weth, To: dai, Fee: 3000}}

	tx, err := SwapCalldata(v3Dex(RouterStyleV1), hops, amountIn, minOut, user, deadline, false, false)
	if err != nil {
		t.Fatal(err)
	}
	vals := decodeCall(t, "exactInput((bytes,address,uint256,uint256,uint256))", tx.Data)
	p := vals[0].Tuple
	if len(p[0].Bytes) != 66 {
		t.Errorf("多跳路径长度 = %d，期望 66", len(p[0].Bytes))
	}
	if !strings.EqualFold(p[1].Addr, user) || p[3].Int().Cmp(amountIn) != 0 || p[4].Int().Cmp(minOut) != 0 {
		t.Error("多跳参数不对")
	}

	tx, err = SwapCalldata(v3Dex(RouterStyleV2), hops, amountIn, minOut, user, deadline, false, false)
	if err != nil {
		t.Fatal(err)
	}
	decodeCall(t, "exactInput((bytes,address,uint256,uint256))", tx.Data)
}

// 换成原生币时，V3 要把「换出包装币」和「解包转给用户」放进同一笔交易里。
func TestSwapCalldataV3NativeOut(t *testing.T) {
	amountIn := big.NewInt(1000000)
	minOut := big.NewInt(990000)
	const deadline = 1700000000
	hops := []Hop{{From: usdc, To: NativePlaceholder, Fee: 3000}}

	tx, err := SwapCalldata(v3Dex(RouterStyleV1), hops, amountIn, minOut, user, deadline, false, true)
	if err != nil {
		t.Fatal(err)
	}
	vals := decodeCall(t, "multicall(uint256,bytes[])", tx.Data)
	if vals[0].Int().Uint64() != deadline {
		t.Error("外层 multicall 没带上过期时间")
	}
	subs := vals[1].Array
	if len(subs) != 2 {
		t.Fatalf("multicall 里应该有 2 个子调用，实际 %d", len(subs))
	}
	// 第一个子调用：换成包装币，收货地址先填路由合约自己
	inner := decodeCall(t, "exactInputSingle((address,address,uint24,address,uint256,uint256,uint256,uint160))", subs[0].Bytes)
	if !strings.EqualFold(inner[0].Tuple[1].Addr, weth) {
		t.Error("换成原生币时买入的应该是包装币")
	}
	if !strings.EqualFold(inner[0].Tuple[3].Addr, tx.To) {
		t.Error("解包前包装币应该先到路由合约手上")
	}
	// 第二个子调用：解包并转给用户
	unwrap := decodeCall(t, "unwrapWETH9(uint256,address)", subs[1].Bytes)
	if unwrap[0].Int().Cmp(minOut) != 0 || !strings.EqualFold(unwrap[1].Addr, user) {
		t.Error("解包参数不对")
	}
}

func TestSwapCalldataV3NativeIn(t *testing.T) {
	amountIn := big.NewInt(1000000)
	tx, err := SwapCalldata(v3Dex(RouterStyleV2), []Hop{{From: NativePlaceholder, To: usdc, Fee: 500}},
		amountIn, big.NewInt(1), user, 1700000000, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Value.Cmp(amountIn) != 0 {
		t.Errorf("原生币买入要附带 value，实际 %s", tx.Value)
	}
	vals := decodeCall(t, "exactInputSingle((address,address,uint24,address,uint256,uint256,uint160))", tx.Data)
	if !strings.EqualFold(vals[0].Tuple[0].Addr, weth) {
		t.Error("原生币买入时卖出的应该是包装币")
	}
}

func TestSwapCalldataRejectsBadInput(t *testing.T) {
	dex := v2Dex()
	if _, err := SwapCalldata(dex, nil, big.NewInt(1), big.NewInt(1), user, 1, false, false); err == nil {
		t.Error("空路线应该报错")
	}
	if _, err := SwapCalldata(dex, []Hop{{From: NativePlaceholder, To: NativePlaceholder}}, big.NewInt(1), big.NewInt(1), user, 1, true, true); err == nil {
		t.Error("原生币换原生币应该报错")
	}
	if _, err := SwapCalldata(dex, []Hop{{From: usdc, To: weth}}, big.NewInt(1), big.NewInt(1), "0x123", 1, false, false); err == nil {
		t.Error("收货地址不合法应该报错")
	}
	// 配置里没填包装合约时，原生币进出都要明确报错，不能拼出一笔发不出去的交易
	noWrap := Dex{ID: "x", Kind: KindV2, Router: "0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"}
	if _, err := SwapCalldata(noWrap, []Hop{{From: NativePlaceholder, To: usdc}}, big.NewInt(1), big.NewInt(1), user, 1, true, false); err == nil {
		t.Error("缺包装合约地址时应该报错")
	}
	if _, err := SwapCalldata(Dex{ID: "y", Kind: "v9"}, []Hop{{From: usdc, To: weth}}, big.NewInt(1), big.NewInt(1), user, 1, false, false); err == nil {
		t.Error("不认识的交易所类型应该报错")
	}
}

func TestTokenHelpers(t *testing.T) {
	hops := []Hop{{From: usdc, To: weth}, {From: weth, To: dai}}
	toks := Tokens(hops)
	if len(toks) != 3 || toks[0] != usdc || toks[2] != dai {
		t.Errorf("Tokens = %v", toks)
	}
	if Tokens(nil) != nil {
		t.Error("空路线应该没有代币")
	}
	if RouteKey(hops) == RouteKey([]Hop{{From: usdc, To: dai}}) {
		t.Error("不同路线不该有同一个 key")
	}
	// 大小写不同但这同一条路线
	if RouteKey(hops) != RouteKey([]Hop{{From: strings.ToLower(usdc), To: strings.ToLower(weth)}, {From: strings.ToLower(weth), To: strings.ToLower(dai)}}) {
		t.Error("同一条路线的大小写不同写法应该算同一个 key")
	}
}

func TestApproveAndQueryCalldata(t *testing.T) {
	data, err := ApproveCalldata(usdc, v2Dex().Router, big.NewInt(12345))
	if err != nil {
		t.Fatal(err)
	}
	vals := decodeCall(t, "approve(address,uint256)", data)
	if !strings.EqualFold(vals[0].Addr, v2Dex().Router) || vals[1].Int().Uint64() != 12345 {
		t.Error("approve 参数不对")
	}

	caller := func(ctx context.Context, calls []Call) ([]Result, error) {
		out := make([]Result, len(calls))
		for i, c := range calls {
			switch {
			case strings.HasPrefix(evmabi.Hex(c.Data), evmabi.Hex(evmabi.Selector("allowance(address,address)"))):
				out[i] = Result{Data: abiUint(999)}
			default:
				out[i] = Result{Data: abiUint(111)}
			}
		}
		return out, nil
	}
	aq, err := AllowanceCalldata(user, v2Dex().Router)
	if err != nil {
		t.Fatal(err)
	}
	bq, err := BalanceCalldata(user)
	if err != nil {
		t.Fatal(err)
	}
	res, err := caller(context.Background(), []Call{{To: usdc, Data: aq}, {To: usdc, Data: bq}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Data == nil || res[1].Data == nil {
		t.Fatal("两个查询都该有返回")
	}
	if v, _ := evmabi.Decode([]evmabi.Type{evmabi.MustParseType("uint256")}, res[0].Data); v[0].Int().Uint64() != 999 {
		t.Error("allowance 解码不对")
	}
}

func TestDexActive(t *testing.T) {
	d := v2Dex()
	if !d.Active() {
		t.Error("没写 enabled 字段时应该默认启用")
	}
	no := false
	d.Enabled = &no
	if d.Active() {
		t.Error("显式关掉的交易所不该是启用状态")
	}
}

func abiUint(n uint64) []byte {
	return evmabi.EncodeUint(n)
}

func abiUint4(a, b, c, d uint64) []byte {
	return append(append(append(evmabi.EncodeUint(a), evmabi.EncodeUint(b)...), evmabi.EncodeUint(c)...), evmabi.EncodeUint(d)...)
}

func abiUintArray(ns []uint64) []byte {
	body := evmabi.EncodeUint(uint64(len(ns)))
	for _, n := range ns {
		body = append(body, evmabi.EncodeUint(n)...)
	}
	return append(evmabi.EncodeUint(32), body...)
}
