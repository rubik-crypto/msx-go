package chainkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// 测试地址一律带十六进制字母：全数字的地址做大小写归一化是空操作，
// 会让「Hex() 返回校验和形式」这类 bug 在单测里隐身。
const (
	testUSDT  = "0x55d398326f99059ff775485246999027b3197955"
	testAddrA = "0xa0b1c2d3e4f5061728394a5b6c7d8e9f00112233"
	testAddrB = "0xb1c2d3e4f5061728394a5b6c7d8e9f0011223344"
	testOther = "0xc2d3e4f5061728394a5b6c7d8e9f001122334455"
	testGas   = "0xd3e4f5061728394a5b6c7d8e9f00112233445566"
)

// newTestEVM 建一个指向假节点的 EVM 数据源
func newTestEVM(t *testing.T, node *fakeNode, cfg Config) *evmSource {
	t.Helper()
	cfg.RPCs = []string{node.URL()}
	if cfg.Chain == "" {
		cfg.Chain = "test"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	applyDefaults(&cfg)
	src, err := newEVMSource(&cfg)
	if err != nil {
		t.Fatalf("建 EVM 数据源失败: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// makeLog 造一条 ERC20 Transfer 日志（用真实类型序列化，编码细节交给 go-ethereum）
func makeLog(block uint64, index uint, contract, from, to string, amount *big.Int) types.Log {
	return types.Log{
		Address:     common.HexToAddress(contract),
		Topics:      []common.Hash{transferTopic, addrTopic(from), addrTopic(to)},
		Data:        common.LeftPadBytes(amount.Bytes(), 32),
		BlockNumber: block,
		TxHash:      common.BigToHash(new(big.Int).SetUint64(block*1000 + uint64(index))),
		TxIndex:     index,
		BlockHash:   common.BigToHash(new(big.Int).SetUint64(block)),
		Index:       index,
	}
}

// blockJSON 造一个 eth_getBlockByNumber 的返回体
func blockJSON(number uint64, txs ...map[string]any) map[string]any {
	return map[string]any{
		"number":       hexutil.Uint64(number),
		"timestamp":    hexutil.Uint64(1_700_000_000 + number),
		"transactions": txs,
	}
}

// txJSON 造一笔区块内交易
func txJSON(hash, from, to string, value int64) map[string]any {
	out := map[string]any{
		"hash":  common.HexToHash(hash),
		"from":  common.HexToAddress(from),
		"value": (*hexutil.Big)(big.NewInt(value)),
	}
	if to != "" {
		out["to"] = common.HexToAddress(to)
	}
	return out
}

func hexToUint(t *testing.T, v any) uint64 {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("期望十六进制字符串，实际 %T(%v)", v, v)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", s, err)
	}
	return n
}

func collect(emits *[]*Transfer) func(*Transfer) error {
	return func(tr *Transfer) error {
		*emits = append(*emits, tr)
		return nil
	}
}

// TestGetLogsFilterArg 请求参数必须手工拼：go-ethereum 的 FilterQuery 没有 MarshalJSON，
// 字段名是大写的，直接丢给 JSON-RPC 会产出非法参数。
func TestGetLogsFilterArg(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(*fakeNode, string, []json.RawMessage) (any, *rpcError) {
		return []types.Log{}, nil
	}
	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA, testAddrB} },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 100, 200, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	calls := node.callsOf("eth_getLogs")
	if len(calls) != 1 {
		t.Fatalf("应当只发一次 eth_getLogs，实际 %d 次", len(calls))
	}
	arg := paramObject(t, calls[0])

	if got := hexToUint(t, arg["fromBlock"]); got != 100 {
		t.Errorf("fromBlock = %d，期望 100", got)
	}
	if got := hexToUint(t, arg["toBlock"]); got != 200 {
		t.Errorf("toBlock = %d，期望 200", got)
	}

	// 必须限定代币合约，否则任意合约伪造 Transfer 日志就能凭空入账
	contracts, ok := arg["address"].([]any)
	if !ok || len(contracts) != 1 || !strings.EqualFold(fmt.Sprint(contracts[0]), testUSDT) {
		t.Errorf("address 参数不对: %v", arg["address"])
	}

	topics, ok := arg["topics"].([]any)
	if !ok || len(topics) != 3 {
		t.Fatalf("topics 应为 3 层，实际 %v", arg["topics"])
	}
	sig, ok := topics[0].([]any)
	if !ok || len(sig) != 1 || !strings.EqualFold(fmt.Sprint(sig[0]), transferTopic.Hex()) {
		t.Errorf("topics[0] 应为 Transfer 签名: %v", topics[0])
	}
	// 这一条是关键回归：中间位必须是 null。空数组虽然语义相同，但部分节点会拒收。
	if topics[1] != nil {
		t.Errorf("topics[1]（from 位）必须是 null 通配，实际 %#v", topics[1])
	}
	recipients, ok := topics[2].([]any)
	if !ok || len(recipients) != 2 {
		t.Fatalf("topics[2] 应带 2 个收款地址，实际 %v", topics[2])
	}
	want := map[string]bool{
		strings.ToLower(addrTopic(testAddrA).Hex()): true,
		strings.ToLower(addrTopic(testAddrB).Hex()): true,
	}
	for _, r := range recipients {
		key := strings.ToLower(fmt.Sprint(r))
		if !want[key] {
			t.Errorf("topics[2] 出现了没关注过的地址: %s", key)
		}
		if len(key) != 66 {
			t.Errorf("地址 topic 必须左补零到 32 字节，实际 %s", key)
		}
	}
}

// TestScanRangeShrinksOnRangeLimit 节点说区间太大时要二分重查，且一条都不能丢
func TestScanRangeShrinksOnRangeLimit(t *testing.T) {
	node := newFakeNode(t, 56)

	// 每 5 个块一条转账，块号 5,10,15,20,25
	logsByBlock := map[uint64][]types.Log{}
	for b := uint64(5); b <= 25; b += 5 {
		logsByBlock[b] = []types.Log{makeLog(b, 0, testUSDT, testOther, testAddrA, big.NewInt(1000))}
	}

	const maxSpan = 10
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		var arg map[string]any
		if err := json.Unmarshal(params[0], &arg); err != nil {
			t.Errorf("解析过滤参数失败: %v", err)
			return nil, nil
		}
		from, _ := strconv.ParseUint(strings.TrimPrefix(fmt.Sprint(arg["fromBlock"]), "0x"), 16, 64)
		to, _ := strconv.ParseUint(strings.TrimPrefix(fmt.Sprint(arg["toBlock"]), "0x"), 16, 64)
		if to-from+1 > maxSpan {
			return nil, &rpcError{Code: -32005, Message: "query returned more than 10000 results"}
		}
		var out []types.Log
		for b := from; b <= to; b++ {
			out = append(out, logsByBlock[b]...)
		}
		if out == nil {
			out = []types.Log{}
		}
		return out, nil
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 1, 25, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	got := map[uint64]int{}
	for _, tr := range emits {
		got[tr.Block]++
	}
	for b := uint64(5); b <= 25; b += 5 {
		if got[b] != 1 {
			t.Errorf("区块 %d 的转账应恰好出现在结果里一次，实际 %d 次", b, got[b])
		}
	}
	if len(emits) != 5 {
		t.Errorf("应当共 5 笔转账，实际 %d", len(emits))
	}

	// 撞到上限之后，安全区间要真的被收窄
	if chunk := src.chunkSize(); chunk > maxSpan {
		t.Errorf("撞到区间上限后安全区间应被收窄到 %d 以内，实际 %d", maxSpan, chunk)
	}
}

// TestScanRangeDoesNotShrinkOnThrottle 限流是瞬态的，正确反应是退避，
// 拆小请求只会成倍放大请求量、把限流撞得更死
func TestScanRangeDoesNotShrinkOnThrottle(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(*fakeNode, string, []json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{Code: -32005, Message: "Too Many Requests"}
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
		Batch:     50,
	})
	before := src.chunkSize()

	var emits []*Transfer
	err := src.ScanRange(context.Background(), 1, 50, collect(&emits))
	if err == nil {
		t.Fatal("限流时应当返回错误，交给外层退避")
	}
	if !IsThrottled(err) {
		t.Errorf("错误应被识别为限流: %v", err)
	}
	if calls := node.callsOf("eth_getLogs"); len(calls) != 1 {
		t.Errorf("限流时不该拆分成多次请求，实际发了 %d 次", len(calls))
	}
	if src.chunkSize() != before {
		t.Errorf("限流不该收窄查询区间：%d → %d", before, src.chunkSize())
	}
}

// TestScanRangeSplitsAddresses 地址太多时要分片查询并合并去重
func TestScanRangeSplitsAddresses(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(*fakeNode, string, []json.RawMessage) (any, *rpcError) {
		return []types.Log{}, nil
	}

	// 比单次上限多一个，逼出第二次查询
	addrs := make([]string, 0, maxAddrsPerQuery+1)
	for i := 0; i <= maxAddrsPerQuery; i++ {
		addrs = append(addrs, common.BigToAddress(big.NewInt(int64(i+1))).Hex())
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return addrs },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 1, 2, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	calls := node.callsOf("eth_getLogs")
	if len(calls) != 2 {
		t.Fatalf("%d 个地址应当分成 2 次查询，实际 %d 次", len(addrs), len(calls))
	}

	seen := map[string]bool{}
	for _, c := range calls {
		topics, _ := paramObject(t, c)["topics"].([]any)
		recipients, _ := topics[2].([]any)
		for _, r := range recipients {
			key := strings.ToLower(fmt.Sprint(r))
			if seen[key] {
				t.Errorf("地址 %s 在多次查询里重复出现", key)
			}
			seen[key] = true
		}
	}
	if len(seen) != len(addrs) {
		t.Errorf("两次查询合计应覆盖 %d 个地址，实际 %d", len(addrs), len(seen))
	}
}

// TestScanRangeDropsUnwatchedRecipient 节点没按 topics 过滤时的兜底：
// 收款方不是我们的地址就不算充值
func TestScanRangeDropsUnwatchedRecipient(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, _ []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		return []types.Log{
			makeLog(10, 0, testUSDT, testOther, testAddrA, big.NewInt(1)),    // 我们的
			makeLog(10, 1, testUSDT, testOther, testOther, big.NewInt(2)),    // 别人的
			makeLog(10, 2, testUSDT, testOther, testAddrA, big.NewInt(0)),    // 金额为 0
			makeLog(10, 3, testOther, testOther, testAddrA, big.NewInt(3)),   // 没配置的合约
			removedLog(10, 4, testUSDT, testOther, testAddrA, big.NewInt(4)), // 已回滚
		}, nil
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 10, 10, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(emits) != 1 {
		t.Fatalf("只应识别出 1 笔有效充值，实际 %d 笔: %+v", len(emits), emits)
	}
	if emits[0].Index != 0 || emits[0].Human() != "0.000000000000000001" {
		t.Errorf("识别出的转账不对: %+v", emits[0])
	}
	if emits[0].Kind != KindToken {
		t.Errorf("Kind 应为 %s，实际 %s", KindToken, emits[0].Kind)
	}
	if emits[0].Timestamp == 0 {
		t.Error("时间戳应当被补齐")
	}
}

// removedLog 造一条标记为已回滚的日志
func removedLog(block uint64, index uint, contract, from, to string, amount *big.Int) types.Log {
	lg := makeLog(block, index, contract, from, to, amount)
	lg.Removed = true
	return lg
}

// TestScanRangeNativeFiltering 原生币只能逐块取交易本地过滤
func TestScanRangeNativeFiltering(t *testing.T) {
	node := newFakeNode(t, 56)
	blocks := map[uint64]map[string]any{
		1: blockJSON(1,
			txJSON("0xa1", testOther, testAddrA, 100), // 命中
			txJSON("0xa2", testOther, testOther, 5),   // 收款方不是我们
			txJSON("0xa3", testOther, "", 1),          // 合约创建
		),
		2: blockJSON(2,
			txJSON("0xb1", testOther, testAddrA, 0), // 金额为 0
			txJSON("0xb2", testGas, testAddrA, 50),  // gas 钱包补气，不是充值
			txJSON("0xb3", testOther, testAddrA, 7), // 命中
		),
	}
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method != "eth_getBlockByNumber" {
			return nil, nil
		}
		var number hexutil.Uint64
		if err := json.Unmarshal(params[0], &number); err != nil {
			t.Errorf("解析块号失败: %v", err)
			return nil, nil
		}
		blk, ok := blocks[uint64(number)]
		if !ok {
			return nil, &rpcError{Code: -32000, Message: "block not found"}
		}
		return blk, nil
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "BNB", Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
		GasWallet: testGas,
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 1, 2, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(emits) != 2 {
		t.Fatalf("应识别出 2 笔原生充值，实际 %d 笔: %+v", len(emits), emits)
	}

	first, second := emits[0], emits[1]
	if first.Block != 1 || first.Index != 0 || first.Amount.Int64() != 100 {
		t.Errorf("第一笔不对: %+v", first)
	}
	if second.Block != 2 || second.Index != 2 || second.Amount.Int64() != 7 {
		t.Errorf("第二笔不对: %+v", second)
	}
	for _, tr := range emits {
		if tr.Kind != KindNative {
			t.Errorf("Kind 应为 %s，实际 %s", KindNative, tr.Kind)
		}
		if tr.Timestamp != int64(1_700_000_000+tr.Block) {
			t.Errorf("区块 %d 的时间戳不对: %d", tr.Block, tr.Timestamp)
		}
	}
}

// TestScanRangeNoAddressesIsNoop 没有关注地址时不该打任何 RPC
func TestScanRangeNoAddressesIsNoop(t *testing.T) {
	node := newFakeNode(t, 56)
	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "BNB", Decimals: 18}, {Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return nil },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 1, 100, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if n := len(node.callsOf("eth_getLogs")) + len(node.callsOf("eth_getBlockByNumber")); n != 0 {
		t.Errorf("没有关注地址时不该扫块或查日志，实际 %d 次", n)
	}
}

// TestScanRangePropagatesEmitError 回调失败必须一路传出去，否则游标会以为这块处理完了
func TestScanRangePropagatesEmitError(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, _ []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		return []types.Log{makeLog(10, 0, testUSDT, testOther, testAddrA, big.NewInt(1))}, nil
	}
	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
	})

	sentinel := errors.New("入账失败")
	err := src.ScanRange(context.Background(), 10, 10, func(*Transfer) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("应当原样返回回调的错误，实际 %v", err)
	}
}

// receiptsFor 造一个区块的收据（用真实类型序列化，编码细节交给 go-ethereum）
func receiptsFor(block uint64, logs ...types.Log) []*types.Receipt {
	rc := &types.Receipt{
		Type:              2,
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: 21000,
		GasUsed:           21000,
		BlockNumber:       new(big.Int).SetUint64(block),
		BlockHash:         common.BigToHash(new(big.Int).SetUint64(block)),
		Logs:              make([]*types.Log, 0, len(logs)),
	}
	for i := range logs {
		rc.TxHash = logs[i].TxHash
		rc.Logs = append(rc.Logs, &logs[i])
	}
	return []*types.Receipt{rc}
}

// TestScanRangeFallsBackToReceipts 节点不支持 eth_getLogs 时改走收据路径
func TestScanRangeFallsBackToReceipts(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		switch method {
		case "eth_getLogs":
			return nil, &rpcError{Code: -32000, Message: "The method eth_getLogs is not supported."}
		case "eth_getBlockReceipts":
			var number hexutil.Uint64
			if err := json.Unmarshal(params[0], &number); err != nil {
				t.Errorf("解析块号失败: %v", err)
				return nil, nil
			}
			return receiptsFor(uint64(number), makeLog(uint64(number), 0, testUSDT, testOther, testAddrA, big.NewInt(500))), nil
		}
		return nil, nil
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 100, 102, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(emits) != 3 {
		t.Fatalf("收据路径应识别出 3 笔转账，实际 %d 笔: %+v", len(emits), emits)
	}
	for i, tr := range emits {
		if tr.Block != uint64(100+i) || tr.Amount.Int64() != 500 {
			t.Errorf("第 %d 笔不对: %+v", i, tr)
		}
	}
	if n := len(node.callsOf("eth_getBlockReceipts")); n != 3 {
		t.Errorf("应当按块取 3 次收据，实际 %d 次", n)
	}
}

// TestReceiptsPathShrinksBatchOnBadRequest 收据批量被节点以体积为由拒掉时要逐级减半。
// 实测某公共节点 12 个区块的批量回 400、2 个正常。
func TestReceiptsPathShrinksBatchOnBadRequest(t *testing.T) {
	node := newFakeNode(t, 56)
	node.batchLimit = 2

	logsByBlock := map[uint64]types.Log{}
	for b := uint64(100); b <= 104; b++ {
		logsByBlock[b] = makeLog(b, 0, testUSDT, testOther, testAddrA, big.NewInt(int64(b)))
	}

	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		switch method {
		case "eth_getLogs":
			return nil, &rpcError{Code: -32000, Message: "method not found"}
		case "eth_getBlockReceipts":
			var number hexutil.Uint64
			_ = json.Unmarshal(params[0], &number)
			return receiptsFor(uint64(number), logsByBlock[uint64(number)]), nil
		}
		return nil, nil
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 100, 104, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(emits) != 5 {
		t.Fatalf("缩批之后应当仍能扫全 5 个区块，实际 %d 笔: %+v", len(emits), emits)
	}
	// 首次 5 个块一批被拒 → 降到 2 → 2+2+1 三批成功
	if n := len(node.callsOf("eth_getBlockReceipts")); n != 5 {
		t.Errorf("应当共取 5 个区块的收据，实际 %d 次", n)
	}
}

// TestBlockBatchShrinksOnRejection 批量取块被拒时要缩批重试，否则补时间戳这一步
// 会让整段扫描失败——代币路径拿到日志之后必然要再问一次区块
func TestBlockBatchShrinksOnRejection(t *testing.T) {
	node := newFakeNode(t, 56)
	node.batchLimit = 2
	logs := make([]types.Log, 0, 5)
	for b := uint64(200); b <= 204; b++ {
		logs = append(logs, makeLog(b, 0, testUSDT, testOther, testAddrA, big.NewInt(9)))
	}
	node.handle = func(_ *fakeNode, method string, _ []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		return logs, nil
	}

	src := newTestEVM(t, node, Config{
		Chain:       "bsc",
		Tokens:      []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses:   func() []string { return []string{testAddrA} },
		NativeBatch: 6, // 首次会按 6 个一批去要区块，节点只接受 2
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 200, 204, collect(&emits)); err != nil {
		t.Fatalf("批量被拒后应当缩批重试，实际失败: %v", err)
	}
	if len(emits) != 5 {
		t.Fatalf("应当识别出 5 笔转账，实际 %d", len(emits))
	}
	if chunk := src.blockChunkSize(); chunk > 2 {
		t.Errorf("批量取块上限应被收窄到 2 以内，实际 %d", chunk)
	}
}

// TestBlockRangeAcceptsHashOnlyTransactions eth_getBlockByNumber 的第二参数为 false 时，
// transactions 是哈希字符串数组而不是交易对象。补时间戳走的正是这条路，
// 解析不了就会让每一段代币扫描都失败。
func TestBlockRangeAcceptsHashOnlyTransactions(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method != "eth_getBlockByNumber" {
			return nil, nil
		}
		var number hexutil.Uint64
		_ = json.Unmarshal(params[0], &number)
		var full bool
		_ = json.Unmarshal(params[1], &full)
		if full {
			return blockJSON(uint64(number), txJSON("0xc1", testOther, testAddrA, 42)), nil
		}
		// 只给哈希，模拟真实节点的精简响应
		return map[string]any{
			"number":       number,
			"timestamp":    hexutil.Uint64(1_700_000_000 + uint64(number)),
			"transactions": []any{common.HexToHash("0xc1").Hex()},
		}, nil
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "BNB", Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 300, 300, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(emits) != 1 || emits[0].Amount.Int64() != 42 {
		t.Fatalf("应当识别出 1 笔 42 的原生充值，实际 %+v", emits)
	}
	if emits[0].Timestamp != 1_700_000_300 {
		t.Errorf("时间戳不对: %d", emits[0].Timestamp)
	}
}

// TestHundredBlocksInOneGetLogs 连扫 100 个区块在代币路径上是**一次** eth_getLogs。
// 这正是代币走 getLogs + topics[2] 而不是逐块取块的意义：请求数不随区块数增长。
func TestHundredBlocksInOneGetLogs(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, _ []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		return []types.Log{}, nil // 没有命中就不会去补时间戳，请求数最能说明问题
	}

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return []string{testAddrA} },
		Batch:     1000,
	})
	before := node.requestCount()

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 1000, 1099, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	if got := node.requestCount() - before; got != 1 {
		t.Errorf("100 个区块应当只发 1 次 HTTP 请求，实际 %d 次", got)
	}
	calls := node.callsOf("eth_getLogs")
	if len(calls) != 1 {
		t.Fatalf("应当只调 1 次 eth_getLogs，实际 %d 次", len(calls))
	}
	arg := paramObject(t, calls[0])
	if got := hexToUint(t, arg["fromBlock"]); got != 1000 {
		t.Errorf("fromBlock = %d，期望 1000", got)
	}
	if got := hexToUint(t, arg["toBlock"]); got != 1099 {
		t.Errorf("toBlock = %d，期望 1099（一个请求要覆盖全部 100 块）", got)
	}
}

// TestHundredBlocksNativeRequestCount 原生币没有按收款方筛交易的接口，
// 只能批量取块本地过滤：100 块按 NativeBatch=20 就是 5 次批量请求
func TestHundredBlocksNativeRequestCount(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method != "eth_getBlockByNumber" {
			return nil, nil
		}
		var number hexutil.Uint64
		_ = json.Unmarshal(params[0], &number)
		return blockJSON(uint64(number)), nil
	}

	src := newTestEVM(t, node, Config{
		Chain:       "bsc",
		Tokens:      []Token{{Symbol: "BNB", Decimals: 18}},
		Addresses:   func() []string { return []string{testAddrA} },
		NativeBatch: 20,
	})
	before := node.requestCount()

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 1000, 1099, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	if got := node.requestCount() - before; got != 5 {
		t.Errorf("100 块按每批 20 个应当发 5 次批量请求，实际 %d 次", got)
	}
	if got := len(node.callsOf("eth_getBlockByNumber")); got != 100 {
		t.Errorf("应当问到 100 个区块，实际 %d", got)
	}
}

// TestTokenSourceAddRemoveTakeEffectNextRound 币种可以运行时增删改：
// TokenSource 每轮重新读一次，所以下一轮就生效，不用重启
func TestTokenSourceAddRemoveTakeEffectNextRound(t *testing.T) {
	const (
		token2    = "0xe4f5061728394a5b6c7d8e9f0011223344556677"
		usdtTenth = "0x55d398326f99059ff775485246999027b3197955"
	)
	ctx := context.Background()

	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, _ []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		// 两个合约各有一条打给关注地址的转账
		return []types.Log{
			makeLog(10, 0, usdtTenth, testOther, testAddrA, big.NewInt(100)),
			makeLog(10, 1, token2, testOther, testAddrA, big.NewInt(200)),
		}, nil
	}

	var mu sync.Mutex
	current := []Token{{Symbol: "USDT", Contract: usdtTenth, Decimals: 6}}
	// 改 decimals 也要下一轮生效，用可变值验证
	src := newTestEVM(t, node, Config{
		Chain: "bsc",
		TokenSource: func() []Token {
			mu.Lock()
			defer mu.Unlock()
			return append([]Token(nil), current...)
		},
		Addresses: func() []string { return []string{testAddrA} },
	})

	scan := func() []*Transfer {
		var out []*Transfer
		if err := src.ScanRange(ctx, 10, 10, collect(&out)); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		return out
	}
	// 最后一次 eth_getLogs 请求里带的代币合约
	lastContracts := func() map[string]bool {
		calls := node.callsOf("eth_getLogs")
		arg := paramObject(t, calls[len(calls)-1])
		out := map[string]bool{}
		for _, c := range arg["address"].([]any) {
			out[strings.ToLower(fmt.Sprint(c))] = true
		}
		return out
	}

	// 第一轮：只监听 USDT
	first := scan()
	if len(first) != 1 || first[0].Token.Symbol != "USDT" {
		t.Fatalf("第一轮应只识别出 USDT，实际 %+v", first)
	}
	if got := lastContracts(); len(got) != 1 || !got[usdtTenth] {
		t.Fatalf("第一轮的 address 过滤应只含 USDT，实际 %v", got)
	}

	// 新增一个代币：下一轮就应该被监听，且过滤条件里带上它的合约
	mu.Lock()
	current = append(current, Token{Symbol: "OCP", Contract: token2, Decimals: 8})
	mu.Unlock()

	second := scan()
	if len(second) != 2 {
		t.Fatalf("新增代币后应识别出 2 笔，实际 %d 笔: %+v", len(second), second)
	}
	if got := lastContracts(); len(got) != 2 || !got[token2] {
		t.Fatalf("新增后 address 过滤应含两个合约，实际 %v", got)
	}

	// 删除 USDT：下一轮不再识别它的转账
	mu.Lock()
	current = current[1:]
	mu.Unlock()

	third := scan()
	if len(third) != 1 || third[0].Token.Symbol != "OCP" {
		t.Fatalf("删除 USDT 后应只剩 OCP，实际 %+v", third)
	}
	if got := lastContracts(); len(got) != 1 || got[usdtTenth] {
		t.Fatalf("删除后 address 过滤里不该再有 USDT，实际 %v", got)
	}
}

// TestTokenSourceOverridesStaticTokens 动态来源优先于静态配置
func TestTokenSourceOverridesStaticTokens(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, _ []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		return []types.Log{makeLog(10, 0, testUSDT, testOther, testAddrA, big.NewInt(1))}, nil
	}

	// 静态配的是另一个币种，不该被用上
	src := newTestEVM(t, node, Config{
		Chain:       "bsc",
		Tokens:      []Token{{Symbol: "别的币", Contract: "0xaaaa...", Decimals: 18}},
		TokenSource: func() []Token { return []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}} },
		Addresses:   func() []string { return []string{testAddrA} },
	})

	var out []*Transfer
	if err := src.ScanRange(context.Background(), 10, 10, collect(&out)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(out) != 1 || out[0].Token.Symbol != "USDT" {
		t.Fatalf("应当以 TokenSource 为准，实际 %+v", out)
	}
}

// TestTokenSourceTogglesNativeScanning 原生币的增删决定要不要走逐块扫这条贵路径
func TestTokenSourceTogglesNativeScanning(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method != "eth_getBlockByNumber" {
			return nil, nil
		}
		var number hexutil.Uint64
		_ = json.Unmarshal(params[0], &number)
		return blockJSON(uint64(number), txJSON("0xd1", testOther, testAddrA, 77)), nil
	}

	var mu sync.Mutex
	watchNative := false
	src := newTestEVM(t, node, Config{
		Chain: "bsc",
		TokenSource: func() []Token {
			mu.Lock()
			defer mu.Unlock()
			if watchNative {
				return []Token{{Symbol: "BNB", Decimals: 18}}
			}
			return nil // 空列表 = 暂不监听任何币种，不报错
		},
		Addresses: func() []string { return []string{testAddrA} },
	})

	var off, on []*Transfer
	if err := src.ScanRange(context.Background(), 5, 5, collect(&off)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(off) != 0 {
		t.Fatalf("没配币种时不该有任何回调，实际 %+v", off)
	}
	if len(node.callsOf("eth_getBlockByNumber")) != 0 {
		t.Error("没配原生币时不该去取区块")
	}

	mu.Lock()
	watchNative = true
	mu.Unlock()

	if err := src.ScanRange(context.Background(), 5, 5, collect(&on)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(on) != 1 || on[0].Kind != KindNative || on[0].Amount.Int64() != 77 {
		t.Fatalf("加上原生币后应识别出原生转账，实际 %+v", on)
	}
}

// TestAddressChunkShrinksWhenRequestTooLarge 关注地址会一直增长，请求体会跟着变大。
// 节点拒收时要把地址分片缩到能用的尺寸，否则一个地址集合涨到某个规模就整体扫不动了。
func TestAddressChunkShrinksWhenRequestTooLarge(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method != "eth_getLogs" {
			return nil, nil
		}
		var arg struct {
			Topics []json.RawMessage `json:"topics"`
		}
		if err := json.Unmarshal(params[0], &arg); err != nil {
			t.Errorf("解析参数失败: %v", err)
			return nil, nil
		}
		var recipients []json.RawMessage
		if len(arg.Topics) >= 3 {
			_ = json.Unmarshal(arg.Topics[2], &recipients)
		}
		// 模拟节点的体积限制：单次最多 4 个地址
		if len(recipients) > 4 {
			return nil, &rpcError{Code: -32600, Message: "request entity too large"}
		}
		return []types.Log{makeLog(10, 0, testUSDT, testOther, testAddrA, big.NewInt(5))}, nil
	}

	// 16 个地址，逼出「整体太大 → 对半拆」这条路径
	addrs := make([]string, 0, 16)
	for i := 0; i < 16; i++ {
		addrs = append(addrs, common.BigToAddress(big.NewInt(int64(i+1))).Hex())
	}
	addrs[0] = testAddrA // 保证日志里的收款方在关注列表里

	src := newTestEVM(t, node, Config{
		Chain:     "bsc",
		Tokens:    []Token{{Symbol: "USDT", Contract: testUSDT, Decimals: 18}},
		Addresses: func() []string { return addrs },
	})

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 10, 10, collect(&emits)); err != nil {
		t.Fatalf("缩分片后应当能扫通，实际失败: %v", err)
	}
	// 每个分片都返回同一条日志，合并时按 (txHash, index) 去重后只剩一条
	if len(emits) != 1 {
		t.Fatalf("分片重复的日志应当合并成 1 条，实际 %d 条", len(emits))
	}
	if got := src.addrChunkSize(); got > 4 {
		t.Errorf("地址分片应被收窄到 4 以内，实际 %d", got)
	}
}

// TestNativeBatchSizeCapsBlockRequests 原生扫块的单批请求数不能超过配置
func TestNativeBatchSizeCapsBlockRequests(t *testing.T) {
	node := newFakeNode(t, 56)
	node.handle = func(_ *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method != "eth_getBlockByNumber" {
			return nil, nil
		}
		var number hexutil.Uint64
		_ = json.Unmarshal(params[0], &number)
		return blockJSON(uint64(number)), nil
	}

	src := newTestEVM(t, node, Config{
		Chain:       "bsc",
		Tokens:      []Token{{Symbol: "BNB", Decimals: 18}},
		Addresses:   func() []string { return []string{testAddrA} },
		NativeBatch: 7,
	})
	before := node.requestCount()

	var emits []*Transfer
	if err := src.ScanRange(context.Background(), 1, 20, collect(&emits)); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	// 20 个块按 7 个一批 → 7+7+6，共 3 次批量请求。
	// 这里数的是 HTTP 请求而不是方法调用：批量请求里的每一笔都会被单独记录。
	if got := node.requestCount() - before; got != 3 {
		t.Errorf("按 NativeBatch=7 取 20 个块应当发 3 次批量请求，实际 %d 次", got)
	}
	if got := len(node.callsOf("eth_getBlockByNumber")); got != 20 {
		t.Errorf("应当问到 20 个区块，实际 %d", got)
	}
}
