// Package dexquote 在多条去中心化交易所之间询价，并把「最优路线」拼成可直接发送的
// 链上交易数据。
//
// 它只做两件事，但都是兑换聚合里最容易写错的两件事：
//
//  1. 报价：按路线（一串代币 + 相邻两两之间的手续费档位）向各个交易所的路由合约
//     或报价合约发起 eth_call，拿到「这么多卖出去能换回多少」。一次请求里会把所有
//     候选路线打包成一批问出去（走 Caller），所以「比价」不是十几个来回的网络等待。
//  2. 拼交易：把选定的路线拼成 swap 的 calldata（含原生币进出、V3 的 multicall
//     解包），以及授权用的 approve calldata。
//
// 交易所被分成两代：
//
//   - V2（恒定乘积，Uniswap V2 那一系）：问到 router.getAmountsOut(uint256,address[]),
//     换出量取返回数组的最后一个。
//   - V3（集中流动性）：问到 QuoterV2.quoteExactInputSingle / quoteExactInput。
//     路由有两代写法（老 SwapRouter 的参数里带 deadline，新的 SwapRouter02 不带），
//     用 Dex.RouterStyle 区分；报价合约的返回值也有两代（QuoterV2 回四个值，
//     Quoter 一代只回一个数），用 Dex.QuoterStyle 区分。
//
// 用法：
//
//	q := dexquote.New(dexquote.HTTPCaller(rpcs, 10*time.Second)) // 也可以换成自己的节点客户端
//	rs := q.QuoteAll(ctx, queries, "latest")        // 一次问完所有候选路线
//	to, data, value, err := dexquote.SwapCalldata(dex, hops, amountIn, minOut, user, deadline, nativeIn, nativeOut)
//
// 不想自己接节点客户端时，直接用 HTTPCaller（见 httpcaller.go），
// 它只用标准库，支持一次批量问多条路线，以及多个节点地址之间的故障转移。
package dexquote

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/rubik-crypto/msx-go/evmabi"
)

// 交易所的两种代次。
const (
	KindV2 = "v2"
	KindV3 = "v3"
)

// V3 路由合约的两代写法。
const (
	RouterStyleV1 = "v1" // 老 SwapRouter：参数里带 deadline
	RouterStyleV2 = "v2" // SwapRouter02：参数里不带 deadline，改由 multicall 传
)

// V3 报价合约的两代返回值。
const (
	QuoterStyleV2 = "v2" // QuoterV2：回 (amountOut, sqrtPriceX96After, ticksCrossed, gasEstimate)
	QuoterStyleV1 = "v1" // Quoter 一代：只回 amountOut
)

// ErrNoPool 表示这条路线在这个交易所上没有可用的池子。
// 这是询价里最常见、也完全正常的一种结果（聚合器就是要跳过它），
// 所以单独给一个哨兵错误，调用方用 errors.Is 判断。
var ErrNoPool = errors.New("这个交易对在该交易所没有可用的池子")

// Call 是一次 eth_call（本包不关心你用什么节点客户端，只要求能批量问）。
type Call struct {
	To   string
	Data []byte
}

// Result 是一次 eth_call 的结果。
type Result struct {
	Data []byte
	Err  error
}

// BatchCaller 把一批 eth_call 一次问出去。实现方要保证返回顺序与入参一致。
type BatchCaller func(ctx context.Context, calls []Call) ([]Result, error)

// Dex 描述一个交易所的一条路由。
type Dex struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`         // v2 / v3
	Router      string   `json:"router"`       // 路由合约地址
	Quoter      string   `json:"quoter"`       // v3 的报价合约地址
	QuoterStyle string   `json:"quoter_style"` // v3：v1 / v2（默认 v2）
	RouterStyle string   `json:"router_style"` // v3：v1 / v2（默认 v1）
	Fees        []uint32 `json:"fees"`         // v3：手续费档位，单位 1e-6（500 = 0.05%）
	Wrapped     string   `json:"wrapped"`      // 该链原生币的包装合约（用原生币兑换时走它）
	Enabled     *bool    `json:"enabled,omitempty"`
}

// Active 表示这个交易所在配置里是不是启用的（默认启用）。
func (d Dex) Active() bool { return d.Enabled == nil || *d.Enabled }

// Hop 是路线里的一跳。
type Hop struct {
	From string `json:"from"`
	To   string `json:"to"`
	Fee  uint32 `json:"fee,omitempty"` // v3 的手续费档位；v2 忽略
}

// Tokens 返回这条路线经过的代币（含首尾）。
func Tokens(hops []Hop) []string {
	if len(hops) == 0 {
		return nil
	}
	out := make([]string, 0, len(hops)+1)
	out = append(out, hops[0].From)
	for _, h := range hops {
		out = append(out, h.To)
	}
	return out
}

// RouteKey 是路线在同一交易所内的唯一标识，用来去重。
func RouteKey(hops []Hop) string {
	parts := make([]string, 0, len(hops))
	for _, h := range hops {
		parts = append(parts, fmt.Sprintf("%s>%s@%d", strings.ToLower(h.From), strings.ToLower(h.To), h.Fee))
	}
	return strings.Join(parts, "|")
}

// Query 是一次待报价的请求。
type Query struct {
	Dex      Dex
	Hops     []Hop
	AmountIn *big.Int
}

// Quote 是一条路线的报价结果。
type Quote struct {
	DexID     string
	DexName   string
	Hops      []Hop
	AmountIn  *big.Int
	AmountOut *big.Int
	Err       error
}

// Quoter 用它持有的 Caller 完成询价。
type Quoter struct {
	caller BatchCaller
}

// New 造一个询价器。
func New(caller BatchCaller) *Quoter { return &Quoter{caller: caller} }

// QuoteAll 把所有候选路线打包成一批问出去，返回与入参一一对应的结果。
// 单条路线失败（没有池子、合约拒绝）不会影响同批的其它路线。
func (q *Quoter) QuoteAll(ctx context.Context, queries []Query, block string) ([]Quote, error) {
	out := make([]Quote, len(queries))
	if len(queries) == 0 {
		return out, nil
	}
	calls := make([]Call, 0, len(queries))
	index := make([]int, 0, len(queries))
	for i, qq := range queries {
		out[i] = Quote{DexID: qq.Dex.ID, DexName: qq.Dex.Name, Hops: qq.Hops, AmountIn: qq.AmountIn}
		call, err := quoteCall(qq)
		if err != nil {
			out[i].Err = err
			continue
		}
		calls = append(calls, call)
		index = append(index, i)
	}
	if len(calls) == 0 {
		return out, nil
	}
	results, err := q.caller(ctx, calls)
	if err != nil {
		return nil, err
	}
	if len(results) != len(calls) {
		return nil, fmt.Errorf("节点返回了 %d 条结果，问了 %d 条", len(results), len(calls))
	}
	for n, r := range results {
		i := index[n]
		if r.Err != nil {
			out[i].Err = r.Err
			continue
		}
		amount, err := decodeQuote(queries[i].Dex, r.Data)
		if err != nil {
			out[i].Err = err
			continue
		}
		out[i].AmountOut = amount
	}
	return out, nil
}

// wrapNative 把原生币占位符换成包装合约：原生币没有合约地址，
// 无论报价还是发交易，链上那头认的都是 WETH/WBNB 这类包装合约。
func (d Dex) wrapNative(addr string) (string, error) {
	if !isNativePlaceholder(addr) {
		return addr, nil
	}
	if d.Wrapped == "" {
		return "", errors.New("配置里缺少原生币的包装合约地址")
	}
	return d.Wrapped, nil
}

// wrapHops 返回把原生币换成包装合约之后的路线。
func (d Dex) wrapHops(hops []Hop) ([]Hop, error) {
	out := make([]Hop, len(hops))
	for i, h := range hops {
		from, err := d.wrapNative(h.From)
		if err != nil {
			return nil, err
		}
		to, err := d.wrapNative(h.To)
		if err != nil {
			return nil, err
		}
		out[i] = Hop{From: from, To: to, Fee: h.Fee}
	}
	return out, nil
}

func quoteCall(q Query) (Call, error) {
	if q.AmountIn == nil || q.AmountIn.Sign() <= 0 {
		return Call{}, errors.New("卖出数量必须大于 0")
	}
	hops, err := q.Dex.wrapHops(q.Hops)
	if err != nil {
		return Call{}, err
	}
	q.Hops = hops
	switch q.Dex.Kind {
	case KindV2:
		path := Tokens(q.Hops)
		if len(path) < 2 {
			return Call{}, errors.New("v2 路线至少要经过两个代币")
		}
		args := make([]evmabi.Value, 0, len(path))
		for _, t := range path {
			args = append(args, evmabi.Address(t))
		}
		call := evmabi.MustCall("getAmountsOut(uint256,address[])", evmabi.BigUint(q.AmountIn), evmabi.ArrayVal(args...))
		data, err := call.Data()
		if err != nil {
			return Call{}, err
		}
		return Call{To: q.Dex.Router, Data: data}, nil
	case KindV3:
		return v3QuoteCall(q)
	}
	return Call{}, fmt.Errorf("不认识的交易所类型 %q", q.Dex.Kind)
}

// PackPath 把 V3 的路线打成合约要的紧凑格式：地址(20 字节) + 手续费(3 字节) + 地址…
func PackPath(hops []Hop) ([]byte, error) {
	if len(hops) == 0 {
		return nil, errors.New("路线是空的")
	}
	var out []byte
	for i, h := range hops {
		from, err := evmabi.AddressBytes(h.From)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			out = append(out, from...)
		}
		if h.Fee > 0xffffff {
			return nil, fmt.Errorf("手续费档位 %d 超出 3 字节", h.Fee)
		}
		out = append(out, byte(h.Fee>>16), byte(h.Fee>>8), byte(h.Fee))
		to, err := evmabi.AddressBytes(h.To)
		if err != nil {
			return nil, err
		}
		out = append(out, to...)
	}
	return out, nil
}

func v3QuoteCall(q Query) (Call, error) {
	quoter := q.Dex.Quoter
	if quoter == "" {
		quoter = q.Dex.Router
	}
	if len(q.Hops) == 1 && q.Dex.QuoterStyle != QuoterStyleV1 {
		// QuoterV2 的单跳报价：参数是一个元组
		call := evmabi.MustCall("quoteExactInputSingle((address,address,uint256,uint24,uint160))",
			evmabi.TupleVal(
				evmabi.Address(q.Hops[0].From),
				evmabi.Address(q.Hops[0].To),
				evmabi.BigUint(q.AmountIn),
				evmabi.Uint(uint64(q.Hops[0].Fee)),
				evmabi.BigUint(new(big.Int)),
			))
		data, err := call.Data()
		if err != nil {
			return Call{}, err
		}
		return Call{To: quoter, Data: data}, nil
	}
	if len(q.Hops) == 1 {
		// Quoter 一代：参数是平铺的五个
		call := evmabi.MustCall("quoteExactInputSingle(address,address,uint256,uint24,uint256)",
			evmabi.Address(q.Hops[0].From),
			evmabi.Address(q.Hops[0].To),
			evmabi.BigUint(q.AmountIn),
			evmabi.Uint(uint64(q.Hops[0].Fee)),
			evmabi.BigUint(new(big.Int)),
		)
		data, err := call.Data()
		if err != nil {
			return Call{}, err
		}
		return Call{To: quoter, Data: data}, nil
	}
	// 多跳走 quoteExactInput(bytes path, uint256 amountIn)
	packed, err := PackPath(q.Hops)
	if err != nil {
		return Call{}, err
	}
	if q.Dex.QuoterStyle == QuoterStyleV1 {
		call := evmabi.MustCall("quoteExactInput(bytes,uint256)", evmabi.BytesVal(packed), evmabi.BigUint(q.AmountIn))
		data, err := call.Data()
		if err != nil {
			return Call{}, err
		}
		return Call{To: quoter, Data: data}, nil
	}
	call := evmabi.MustCall("quoteExactInput(bytes,uint256)", evmabi.BytesVal(packed), evmabi.BigUint(q.AmountIn))
	data, err := call.Data()
	if err != nil {
		return Call{}, err
	}
	return Call{To: quoter, Data: data}, nil
}

func decodeQuote(d Dex, data []byte) (*big.Int, error) {
	if len(data) == 0 {
		return nil, ErrNoPool
	}
	if d.Kind == KindV2 {
		types := []evmabi.Type{evmabi.MustParseType("uint256[]")}
		vals, err := evmabi.Decode(types, data)
		if err != nil {
			return nil, err
		}
		arr := vals[0].Array
		if len(arr) == 0 {
			return nil, ErrNoPool
		}
		amount := arr[len(arr)-1].Int()
		if amount.Sign() <= 0 {
			return nil, ErrNoPool
		}
		return amount, nil
	}
	if d.QuoterStyle == QuoterStyleV1 {
		types := []evmabi.Type{evmabi.MustParseType("uint256")}
		vals, err := evmabi.Decode(types, data)
		if err != nil {
			return nil, err
		}
		if vals[0].Int().Sign() <= 0 {
			return nil, ErrNoPool
		}
		return vals[0].Int(), nil
	}
	types := []evmabi.Type{
		evmabi.MustParseType("uint256"),
		evmabi.MustParseType("uint160"),
		evmabi.MustParseType("uint32"),
		evmabi.MustParseType("uint256"),
	}
	vals, err := evmabi.Decode(types, data)
	if err != nil {
		// 少数链上部署的是老版 Quoter，回的是单个数值：按单值再解一次
		single := []evmabi.Type{evmabi.MustParseType("uint256")}
		if sv, serr := evmabi.Decode(single, data); serr == nil && sv[0].Int().Sign() > 0 {
			return sv[0].Int(), nil
		}
		return nil, err
	}
	if vals[0].Int().Sign() <= 0 {
		return nil, ErrNoPool
	}
	return vals[0].Int(), nil
}

// ApproveCalldata 拼出「授权给路由合约」的 calldata。
func ApproveCalldata(token, spender string, amount *big.Int) ([]byte, error) {
	call, err := evmabi.NewCall("approve(address,uint256)", evmabi.Address(spender), evmabi.BigUint(amount))
	if err != nil {
		return nil, err
	}
	return call.Data()
}

// AllowanceCalldata 拼出查授权额度的 calldata。
func AllowanceCalldata(owner, spender string) ([]byte, error) {
	call, err := evmabi.NewCall("allowance(address,address)", evmabi.Address(owner), evmabi.Address(spender))
	if err != nil {
		return nil, err
	}
	return call.Data()
}

// BalanceCalldata 拼出查余额的 calldata。
func BalanceCalldata(owner string) ([]byte, error) {
	call, err := evmabi.NewCall("balanceOf(address)", evmabi.Address(owner))
	if err != nil {
		return nil, err
	}
	return call.Data()
}

// SwapTx 是一笔可以交给钱包发送的交易。
type SwapTx struct {
	To    string   // 路由合约
	Data  []byte   // calldata
	Value *big.Int // 附带发送的原生币数量（用原生币买入时不为 0）
}

// SwapCalldata 把路线拼成 swap 交易。
//
// amountIn 是卖出数量（用原生币买时以 wei 计），minOut 是最少到手量（滑点保护），
// recipient 是收货地址，deadline 是过期时间（Unix 秒）。nativeIn/nativeOut 说明
// 卖/买的是不是这条链的原生币——原生币没有合约地址，V2 用的是带 ETH 的专用函数，
// V3 则是拿包装合约当中间人再解包。
func SwapCalldata(d Dex, hops []Hop, amountIn, minOut *big.Int, recipient string, deadline uint64, nativeIn, nativeOut bool) (SwapTx, error) {
	if len(hops) == 0 {
		return SwapTx{}, errors.New("路线是空的")
	}
	if nativeIn && nativeOut {
		return SwapTx{}, errors.New("原生币换原生币没有意义")
	}
	if _, err := evmabi.NormalizeAddress(recipient); err != nil {
		return SwapTx{}, fmt.Errorf("收货地址不对：%w", err)
	}
	wrap := func(addr string) string {
		if isNativePlaceholder(addr) {
			return d.Wrapped
		}
		return addr
	}
	path := Tokens(hops)
	wrappedPath := make([]string, len(path))
	for i, t := range path {
		wrappedPath[i] = wrap(t)
	}
	if nativeIn && wrappedPath[0] == "" {
		return SwapTx{}, errors.New("配置里缺少原生币的包装合约地址")
	}
	if nativeOut && wrappedPath[len(wrappedPath)-1] == "" {
		return SwapTx{}, errors.New("配置里缺少原生币的包装合约地址")
	}
	// V3 的路线里，把参与的原生币换成包装合约
	wh := make([]Hop, len(hops))
	for i, h := range hops {
		wh[i] = Hop{From: wrap(h.From), To: wrap(h.To), Fee: h.Fee}
	}

	switch d.Kind {
	case KindV2:
		return v2SwapTx(d, wrappedPath, amountIn, minOut, recipient, deadline, nativeIn, nativeOut)
	case KindV3:
		return v3SwapTx(d, wh, amountIn, minOut, recipient, deadline, nativeIn, nativeOut)
	}
	return SwapTx{}, fmt.Errorf("不认识的交易所类型 %q", d.Kind)
}

// NativePlaceholder 是配置里代表原生币的写法（原生币没有合约地址）。
const NativePlaceholder = "native"

// isNativePlaceholder 只认显式的 "native" 写法。空地址不是原生币，
// 它是「写漏了」——那应该老老实实报错，而不是被悄悄当成包装合约。
func isNativePlaceholder(addr string) bool {
	return strings.EqualFold(strings.TrimSpace(addr), NativePlaceholder)
}

func v2SwapTx(d Dex, path []string, amountIn, minOut *big.Int, recipient string, deadline uint64, nativeIn, nativeOut bool) (SwapTx, error) {
	addrs := make([]evmabi.Value, 0, len(path))
	for _, t := range path {
		if isNativePlaceholder(t) {
			return SwapTx{}, errors.New("v2 路线里还有没换成包装合约的原生币")
		}
		addrs = append(addrs, evmabi.Address(t))
	}
	pathVal := evmabi.ArrayVal(addrs...)
	var call evmabi.Call
	var value *big.Int
	switch {
	case nativeIn:
		call = evmabi.MustCall("swapExactETHForTokens(uint256,address[],address,uint256)",
			evmabi.BigUint(minOut), pathVal, evmabi.Address(recipient), evmabi.Uint(deadline))
		value = new(big.Int).Set(amountIn)
	case nativeOut:
		call = evmabi.MustCall("swapExactTokensForETH(uint256,uint256,address[],address,uint256)",
			evmabi.BigUint(amountIn), evmabi.BigUint(minOut), pathVal, evmabi.Address(recipient), evmabi.Uint(deadline))
		value = new(big.Int)
	default:
		call = evmabi.MustCall("swapExactTokensForTokens(uint256,uint256,address[],address,uint256)",
			evmabi.BigUint(amountIn), evmabi.BigUint(minOut), pathVal, evmabi.Address(recipient), evmabi.Uint(deadline))
		value = new(big.Int)
	}
	data, err := call.Data()
	if err != nil {
		return SwapTx{}, err
	}
	return SwapTx{To: d.Router, Data: data, Value: value}, nil
}

func v3SwapTx(d Dex, hops []Hop, amountIn, minOut *big.Int, recipient string, deadline uint64, nativeIn, nativeOut bool) (SwapTx, error) {
	style := d.RouterStyle
	if style == "" {
		style = RouterStyleV1
	}
	innerRecipient := recipient
	if nativeOut {
		// 先把包装币换到路由合约手上，再解包成原生币转给用户
		innerRecipient = d.Router
	}
	var inner evmabi.Call
	var err error
	if len(hops) == 1 {
		h := hops[0]
		if style == RouterStyleV2 {
			inner, err = evmabi.NewCall("exactInputSingle((address,address,uint24,address,uint256,uint256,uint160))",
				evmabi.TupleVal(
					evmabi.Address(h.From), evmabi.Address(h.To), evmabi.Uint(uint64(h.Fee)),
					evmabi.Address(innerRecipient), evmabi.BigUint(amountIn), evmabi.BigUint(minOut),
					evmabi.BigUint(new(big.Int)),
				))
		} else {
			inner, err = evmabi.NewCall("exactInputSingle((address,address,uint24,address,uint256,uint256,uint256,uint160))",
				evmabi.TupleVal(
					evmabi.Address(h.From), evmabi.Address(h.To), evmabi.Uint(uint64(h.Fee)),
					evmabi.Address(innerRecipient), evmabi.Uint(deadline), evmabi.BigUint(amountIn), evmabi.BigUint(minOut),
					evmabi.BigUint(new(big.Int)),
				))
		}
	} else {
		packed, perr := PackPath(hops)
		if perr != nil {
			return SwapTx{}, perr
		}
		if style == RouterStyleV2 {
			inner, err = evmabi.NewCall("exactInput((bytes,address,uint256,uint256))",
				evmabi.TupleVal(
					evmabi.BytesVal(packed), evmabi.Address(innerRecipient),
					evmabi.BigUint(amountIn), evmabi.BigUint(minOut),
				))
		} else {
			inner, err = evmabi.NewCall("exactInput((bytes,address,uint256,uint256,uint256))",
				evmabi.TupleVal(
					evmabi.BytesVal(packed), evmabi.Address(innerRecipient), evmabi.Uint(deadline),
					evmabi.BigUint(amountIn), evmabi.BigUint(minOut),
				))
		}
	}
	if err != nil {
		return SwapTx{}, err
	}
	innerData, err := inner.Data()
	if err != nil {
		return SwapTx{}, err
	}

	value := new(big.Int)
	if nativeIn {
		value = new(big.Int).Set(amountIn)
	}
	if !nativeOut {
		return SwapTx{To: d.Router, Data: innerData, Value: value}, nil
	}

	// 原生币卖出：外面套一层 multicall，把「换成包装币」和「解包并转给用户」放在同一笔里
	unwrap, err := evmabi.NewCall("unwrapWETH9(uint256,address)", evmabi.BigUint(minOut), evmabi.Address(recipient))
	if err != nil {
		return SwapTx{}, err
	}
	unwrapData, err := unwrap.Data()
	if err != nil {
		return SwapTx{}, err
	}
	var outer evmabi.Call
	if style == RouterStyleV2 {
		// SwapRouter02 的 multicall 只有 bytes[]，deadline 由 outer 之外的调用方保证
		outer, err = evmabi.NewCall("multicall(bytes[])", evmabi.ArrayVal(evmabi.BytesVal(innerData), evmabi.BytesVal(unwrapData)))
	} else {
		outer, err = evmabi.NewCall("multicall(uint256,bytes[])", evmabi.Uint(deadline),
			evmabi.ArrayVal(evmabi.BytesVal(innerData), evmabi.BytesVal(unwrapData)))
	}
	if err != nil {
		return SwapTx{}, err
	}
	data, err := outer.Data()
	if err != nil {
		return SwapTx{}, err
	}
	return SwapTx{To: d.Router, Data: data, Value: value}, nil
}

// HexData 返回 0x 开头的 calldata 字符串。
func HexData(data []byte) string { return hex.EncodeToString(data) }

// DefaultFeeTier 从手续费档位里挑一个最常用的（优先 0.3%，也就是 3000），
// 用在「探一下这个交易所还通不通」这类场合。
func DefaultFeeTier(fees []uint32) uint32 {
	if len(fees) == 0 {
		return 3000
	}
	for _, f := range fees {
		if f == 3000 {
			return f
		}
	}
	return fees[len(fees)/2]
}
