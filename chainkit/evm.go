package chainkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	// maxAddrsPerQuery 单次 eth_getLogs 里 topics[2] 能带多少个地址。
	// 协议没有硬上限，但地址数组会等比放大请求体，先撞上的是 provider 的请求体积限制。
	maxAddrsPerQuery = 1000

	// resultLimitHint geth 系 eth_getLogs 的默认结果条数上限。有些节点返回条数超限时
	// 是静默截断而不是报错，所以命中这个数字就当作「可能被截断」处理，缩小区间重查。
	resultLimitHint = 10000

	// nativeBlocksPerBatch 原生币扫块的单批上限，与代币区间分开：
	// 原生币每块都要取回完整交易列表，批量开太大只是把内存峰值推高。
	nativeBlocksPerBatch = 20

	// receiptsBatchCap 收据路径的单批区块数上限。收据比区块头大得多，
	// 就算节点没抱怨，一次拉几十个块的收据也会撑出很大的瞬时内存。
	receiptsBatchCap = 10
)

// evmSource EVM 兼容链的读写数据源。
type evmSource struct {
	cfg  *Config
	pool *rpcPool
	logf func(format string, args ...any)

	gasWallet string // 小写；空串表示不过滤

	mu         sync.Mutex
	chunk      uint64 // 自适应后的代币日志查询区间上限
	blockChunk uint64 // 自适应后的批量取块上限
	addrChunk  int    // 自适应后的单次 eth_getLogs 地址数上限
}

// tokenSet 一轮扫描用的币种快照。
//
// 不把币种表缓存在 evmSource 上，是因为 TokenSource 允许运行时增删改——
// 每轮重新快照一份局部变量传下去，就没有共享可变状态要守，也不存在
// 「扫描中途币种被换掉」这种半路变更的问题。
type tokenSet struct {
	byContract map[string]Token // 小写合约地址 → 代币
	native     *Token           // 原生币，未配置则为 nil
	contracts  []string         // 排序后的合约地址，供 eth_getLogs 的 address 过滤
}

// snapshotTokens 取本轮的币种快照。TokenSource 优先于静态的 Tokens。
func (s *evmSource) snapshotTokens() tokenSet {
	list := s.cfg.Tokens
	if s.cfg.TokenSource != nil {
		list = s.cfg.TokenSource()
	}
	return newTokenSet(list)
}

func newTokenSet(list []Token) tokenSet {
	ts := tokenSet{byContract: make(map[string]Token, len(list))}
	for _, t := range list {
		if t.IsNative() {
			native := t
			ts.native = &native
			continue
		}
		// 同一个合约配了两次时后者覆盖前者；空合约地址已在上面按原生币处理
		ts.byContract[strings.ToLower(strings.TrimSpace(t.Contract))] = t
	}
	ts.contracts = make([]string, 0, len(ts.byContract))
	for c := range ts.byContract {
		ts.contracts = append(ts.contracts, c)
	}
	sort.Strings(ts.contracts)
	return ts
}

func newEVMSource(cfg *Config) (*evmSource, error) {
	pool, err := newRPCPool(context.Background(), cfg.RPCs, cfg.ChainID, cfg.logf)
	if err != nil {
		return nil, err
	}
	return &evmSource{
		cfg:        cfg,
		pool:       pool,
		logf:       cfg.logf,
		chunk:      cfg.batch,
		blockChunk: cfg.nativeBatch,
		addrChunk:  maxAddrsPerQuery,
		gasWallet:  strings.ToLower(strings.TrimSpace(cfg.GasWallet)),
	}, nil
}

// Head 当前链头高度
func (s *evmSource) Head(ctx context.Context) (uint64, error) {
	var n hexutil.Uint64
	if err := s.pool.call(ctx, &n, "eth_blockNumber"); err != nil {
		return 0, err
	}
	return uint64(n), nil
}

// ChainID 链 ID
func (s *evmSource) ChainID(context.Context) (uint64, error) { return s.pool.chainID(), nil }

// Probe 探测首选端点是否已恢复
func (s *evmSource) Probe(ctx context.Context) { s.pool.probePreferred(ctx) }

// Preferred 当前是否在首选端点上
func (s *evmSource) Preferred() bool { return s.pool.preferred() }

// Close 释放连接
func (s *evmSource) Close() error { return s.pool.close() }

// chunkSize 当前的自适应代币查询区间上限
func (s *evmSource) chunkSize() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunk
}

// shrink 撞到区间/结果集上限后把查询区间收窄。只降不升：自动回升只会周期性
// 再撞一次同样的上限，换来一轮必然失败的请求。
func (s *evmSource) shrink(n uint64) {
	next := max(n/2, 1)
	s.mu.Lock()
	if next < s.chunk {
		s.chunk = next
		s.logf("chainkit: eth_getLogs 区间上限收窄至 %d 块", next)
	}
	s.mu.Unlock()
}

// ScanRange 扫描 [from, to] 区间内所有打给关注地址的转账。
//
// 两条路径的成本结构完全不同：
//   - 代币：把「收款方 = 我们的地址」下推给节点（topics[2]），一次请求覆盖一整段区间。
//   - 原生币：EVM 没有任何按收款方筛交易的接口，只能逐块取回完整交易列表再本地过滤，
//     成本随链上交易量线性上涨。所以只有配置了原生币才走这条路。
func (s *evmSource) ScanRange(ctx context.Context, from, to uint64, emit func(*Transfer) error) error {
	if from > to {
		return nil
	}
	// 关注地址与币种都在这里各取一次快照：调用方随时可以增删改，
	// 本轮扫描看到的是本轮开始那一刻的配置。
	addrs := s.watchSet()
	if len(addrs) == 0 {
		return nil // 没有要关注的地址，扫了也没意义
	}
	tokens := s.snapshotTokens()

	if len(tokens.byContract) > 0 {
		if err := s.scanTokens(ctx, from, to, addrs, tokens, emit); err != nil {
			return err
		}
	}
	if tokens.native != nil {
		if err := s.scanNative(ctx, from, to, addrs, *tokens.native, emit); err != nil {
			return err
		}
	}
	return nil
}

// watchSet 读取当前关注地址并归一化成小写地址集合
func (s *evmSource) watchSet() map[string]struct{} {
	raw := s.cfg.Addresses()
	out := make(map[string]struct{}, len(raw))
	for _, a := range raw {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		out[a] = struct{}{}
	}
	return out
}

// ============================================================================
// 代币
// ============================================================================

// scanTokens 按自适应区间分批拉取代币日志
func (s *evmSource) scanTokens(ctx context.Context, from, to uint64, addrs map[string]struct{}, tokens tokenSet, emit func(*Transfer) error) error {
	for start := from; ; {
		end := min(start+s.chunkSize()-1, to)
		logs, err := s.getLogsRange(ctx, start, end, addrs, tokens.contracts)
		if err != nil {
			return err
		}
		if err := s.emitTokenLogs(ctx, logs, addrs, tokens, emit); err != nil {
			return err
		}
		if end >= to {
			return nil
		}
		start = end + 1
	}
}

// emitTokenLogs 把日志翻译成转账并回调
func (s *evmSource) emitTokenLogs(ctx context.Context, logs []types.Log, addrs map[string]struct{}, tokens tokenSet, emit func(*Transfer) error) error {
	if len(logs) == 0 {
		return nil
	}
	// 节点通常按 (块, 日志序号) 返回，但不做保证；本地排一次让回调顺序确定
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		return logs[i].Index < logs[j].Index
	})

	timestamps, err := s.blockTimestamps(ctx, logs)
	if err != nil {
		return err
	}

	for i := range logs {
		lg := &logs[i]
		if lg.Removed || len(lg.Topics) < 3 || len(lg.Data) < 32 {
			continue
		}
		token, ok := tokens.byContract[strings.ToLower(lg.Address.Hex())]
		if !ok {
			continue // 节点没按 address 过滤时的兜底
		}
		toAddr := topicToAddress(lg.Topics[2])
		if _, ok := addrs[toAddr]; !ok {
			continue // topics[2] 过滤的兜底
		}
		amount := new(big.Int).SetBytes(lg.Data)
		if amount.Sign() <= 0 {
			continue
		}
		if err := emit(&Transfer{
			Chain:     s.cfg.Chain,
			TxHash:    lg.TxHash.Hex(),
			Block:     lg.BlockNumber,
			Kind:      KindToken,
			Index:     uint(lg.Index),
			From:      topicToAddress(lg.Topics[1]),
			To:        toAddr,
			Token:     token,
			Amount:    amount,
			Timestamp: timestamps[lg.BlockNumber],
		}); err != nil {
			return err
		}
	}
	return nil
}

// getLogsRange 取 [from, to] 区间的代币日志，失败时按错误类别走回退阶梯。
//
//	区间/结果集过大 → 二分区间重查（并把安全区间记下来，后续批次直接用小的）
//	方法不支持       → 改走收据路径
//	其它             → 原样上抛（限流交给外层退避，绝不拆小请求）
func (s *evmSource) getLogsRange(ctx context.Context, from, to uint64, addrs map[string]struct{}, contracts []string) ([]types.Log, error) {
	logs, err := s.getLogsChunked(ctx, from, to, addrs, contracts)
	if err == nil {
		// 结果条数正好顶到已知上限：可能是被静默截断，二分重查避免丢数据
		if len(logs) >= resultLimitHint && to > from {
			s.shrink(to - from + 1)
			mid := from + (to-from)/2
			head, err := s.getLogsRange(ctx, from, mid, addrs, contracts)
			if err != nil {
				return nil, err
			}
			tail, err := s.getLogsRange(ctx, mid+1, to, addrs, contracts)
			if err != nil {
				return nil, err
			}
			return append(head, tail...), nil
		}
		return logs, nil
	}

	switch classify(err) {
	case classRangeLimit, classRequestTooLarge:
		// 结果集过大：二分区间。请求体过大一般已经在 getLogsOne 里靠拆地址解决了，
		// 能走到这里说明拆到单个地址还是被拒——剩下的变量就只有区间。
		if from == to {
			return nil, err // 单块都超限，只能交给上层
		}
		s.shrink(to - from + 1)
		mid := from + (to-from)/2
		head, err := s.getLogsRange(ctx, from, mid, addrs, contracts)
		if err != nil {
			return nil, err
		}
		tail, err := s.getLogsRange(ctx, mid+1, to, addrs, contracts)
		if err != nil {
			return nil, err
		}
		return append(head, tail...), nil
	case classMethodMissing:
		s.logf("chainkit: 节点不支持 eth_getLogs（%v），回退到收据路径", err)
		return s.logsByReceipts(ctx, from, to, addrs, contracts)
	default:
		return nil, err
	}
}

// getLogsChunked 一次区间查询，按自适应的大小把地址分片。
// 地址分片是必须的：平台充值地址会一直增长，全塞进一个 topics 数组会撑爆请求体。
func (s *evmSource) getLogsChunked(ctx context.Context, from, to uint64, addrs map[string]struct{}, contracts []string) ([]types.Log, error) {
	addrList := sortedKeys(addrs)
	var out []types.Log
	seen := make(map[string]struct{})

	for _, addrChunk := range chunkStrings(addrList, s.addrChunkSize()) {
		part, err := s.getLogsOne(ctx, from, to, addrChunk, contracts)
		if err != nil {
			return nil, err
		}
		for _, lg := range part {
			key := lg.TxHash.Hex() + ":" + strconv.FormatUint(uint64(lg.Index), 10)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, lg)
		}
	}
	return out, nil
}

// getLogsOne 发一次 eth_getLogs。请求体被节点以「太大」为由拒掉时，把地址列表对半拆开重试。
//
// 为什么只在这一层拆地址：结果集过大（"query exceeds max results"）是区间的问题，
// 由 getLogsRange 去二分区间；而 HTTP 400/413 是请求体本身太大，地址数组正是请求体
// 里最大的一块——1000 个地址就是 66KB。两者必须分开处理，否则要么白拆区间、
// 要么白拆地址。
//
// 拆分成功才把更小的尺寸记下来。记错了会很惨：把「区间太大」误判成地址太多，
// 一路缩到每次只带 1 个地址，就是每轮几百个请求。
func (s *evmSource) getLogsOne(ctx context.Context, from, to uint64, addrs []string, contracts []string) ([]types.Log, error) {
	var part []types.Log
	err := s.pool.call(ctx, &part, "eth_getLogs", s.filterArg(from, to, addrs, contracts))
	if err == nil {
		return part, nil
	}
	if classify(err) != classRequestTooLarge || len(addrs) <= 1 {
		return nil, err
	}

	mid := len(addrs) / 2
	head, err := s.getLogsOne(ctx, from, to, addrs[:mid], contracts)
	if err != nil {
		return nil, err
	}
	tail, err := s.getLogsOne(ctx, from, to, addrs[mid:], contracts)
	if err != nil {
		return nil, err
	}
	// 记的是 mid（真正被验证通过的那个尺寸），不是 len(addrs)——后者刚刚才被拒过
	s.shrinkAddrChunk(mid)
	return append(head, tail...), nil
}

// filterArg 拼 eth_getLogs 的过滤参数。
//
// 手工拼而不是用 ethereum.FilterQuery：后者没有实现 MarshalJSON，字段名是大写的，
// 直接交给 JSON-RPC 会产出非法参数（go-ethereum 内部也是手工拼的）。
func (s *evmSource) filterArg(from, to uint64, addrs []string, contracts []string) map[string]interface{} {
	targets := make([]common.Hash, 0, len(addrs))
	for _, a := range addrs {
		targets = append(targets, addrTopic(a))
	}
	// 中间位（from）留 nil = 通配，序列化成 null；
	// 空切片会 marshal 成 []，部分节点对空数组的容忍度不如 null
	arg := map[string]interface{}{
		"fromBlock": hexutil.Uint64(from),
		"toBlock":   hexutil.Uint64(to),
		"topics":    []interface{}{[]common.Hash{transferTopic}, nil, targets},
	}
	// 必须同时限定代币合约：否则任意合约都能伪造 Transfer(?, 我们的地址, ?) 日志，
	// 那就是凭空入账
	if len(contracts) > 0 {
		arg["address"] = contracts
	}
	return arg
}

// addrChunkSize 当前的单次地址分片大小
func (s *evmSource) addrChunkSize() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addrChunk
}

// shrinkAddrChunk 记住一个被验证过能用的地址分片大小（只降不升）
func (s *evmSource) shrinkAddrChunk(working int) {
	s.mu.Lock()
	if working < s.addrChunk {
		s.addrChunk = working
		s.logf("chainkit: eth_getLogs 单次地址数上限收窄至 %d", working)
	}
	s.mu.Unlock()
}

// logsByReceipts 收据路径：节点不支持 eth_getLogs 时的兜底，
// 拉区块收据再从里面筛 Transfer 日志。
//
// 这条路的成本远高于 eth_getLogs：要取回区间内每个区块的**全部**收据。所以批大小
// 单独设上限，并且撞到「批量过大」时逐级减半——不少节点的限制正好卡在收据体积上
// （实测某公共节点 12 个区块的批量回 400，2 个正常）。
func (s *evmSource) logsByReceipts(ctx context.Context, from, to uint64, addrs map[string]struct{}, contracts []string) ([]types.Log, error) {
	allowed := make(map[string]struct{}, len(contracts))
	for _, c := range contracts {
		allowed[strings.ToLower(c)] = struct{}{}
	}

	var out []types.Log
	size := min(s.chunkSize(), receiptsBatchCap)
	for start := from; ; {
		end := min(start+size-1, to)
		logs, err := s.receiptsBatch(ctx, start, end, addrs, allowed)
		if err != nil {
			if size > 1 && isBatchTooLarge(err) {
				size = max(size/2, 1)
				s.logf("chainkit: eth_getBlockReceipts 批量被拒（%v），批大小降到 %d 块", err, size)
				continue // 不推进 start：用更小的批重试同一段
			}
			return nil, err
		}
		out = append(out, logs...)
		if end >= to {
			return out, nil
		}
		start = end + 1
	}
}

// receiptsBatch 取 [from, to] 区间的收据并筛出命中的 Transfer 日志
func (s *evmSource) receiptsBatch(ctx context.Context, from, to uint64, addrs, allowed map[string]struct{}) ([]types.Log, error) {
	nums := make([]uint64, 0, to-from+1)
	for n := from; n <= to; n++ {
		nums = append(nums, n)
	}

	elems := make([]rpc.BatchElem, len(nums))
	results := make([][]*types.Receipt, len(nums))
	for i, n := range nums {
		elems[i] = rpc.BatchElem{
			Method: "eth_getBlockReceipts",
			Args:   []interface{}{hexutil.Uint64(n)},
			Result: &results[i],
		}
	}
	if err := s.pool.batch(ctx, elems); err != nil {
		return nil, err
	}

	var out []types.Log
	for i := range elems {
		if elems[i].Error != nil {
			if classify(elems[i].Error) == classMethodMissing {
				return nil, errors.New("chainkit: 节点既不支持 eth_getLogs 也不支持 eth_getBlockReceipts，无法扫描代币转账，请换 RPC 端点")
			}
			return nil, fmt.Errorf("获取区块 %d 收据失败: %w", nums[i], elems[i].Error)
		}
		for _, rc := range results[i] {
			if rc == nil {
				continue
			}
			for _, lg := range rc.Logs {
				if lg != nil && s.matchLog(lg, addrs, allowed) {
					out = append(out, *lg)
				}
			}
		}
	}
	return out, nil
}

// matchLog 收据路径的本地筛：代币合约 + 收款方
func (s *evmSource) matchLog(lg *types.Log, addrs map[string]struct{}, allowed map[string]struct{}) bool {
	if lg.Removed || len(lg.Topics) < 3 || len(lg.Data) < 32 || lg.Topics[0] != transferTopic {
		return false
	}
	if _, ok := allowed[strings.ToLower(lg.Address.Hex())]; !ok {
		return false
	}
	_, ok := addrs[topicToAddress(lg.Topics[2])]
	return ok
}

// blockTimestamps 批量补齐涉及区块的时间戳。
// eth_getLogs 的返回里没有时间，只能按去重后的块号再问一次节点。
func (s *evmSource) blockTimestamps(ctx context.Context, logs []types.Log) (map[uint64]int64, error) {
	out := make(map[uint64]int64, len(logs))
	if len(logs) == 0 {
		return out, nil
	}

	seen := make(map[uint64]struct{}, len(logs))
	nums := make([]uint64, 0, len(logs))
	for _, lg := range logs {
		if _, dup := seen[lg.BlockNumber]; dup {
			continue
		}
		seen[lg.BlockNumber] = struct{}{}
		nums = append(nums, lg.BlockNumber)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })

	for start := 0; start < len(nums); start += nativeBlocksPerBatch {
		end := min(start+nativeBlocksPerBatch, len(nums))
		blocks, err := s.blockRange(ctx, nums[start:end], false)
		if err != nil {
			return nil, err
		}
		for i, blk := range blocks {
			out[nums[start+i]] = int64(blk.Timestamp)
		}
	}
	return out, nil
}

// ============================================================================
// 原生币
// ============================================================================

// scanNative 逐块取回完整交易列表，本地筛出打给关注地址的原生转账。
//
// 已知限制：这里不查收据，所以拿不到交易的执行结果。对充值地址（EOA）而言这没有
// 实际影响——往一个没有代码的地址转原生币不可能回滚；但如果把合约地址配成了收款
// 地址，回滚的交易也会被当成充值。需要严格判定时把该地址从 Addresses 里摘掉。
func (s *evmSource) scanNative(ctx context.Context, from, to uint64, addrs map[string]struct{}, native Token, emit func(*Transfer) error) error {
	for start := from; ; {
		end := min(start+s.blockChunkSize()-1, to)
		nums := make([]uint64, 0, end-start+1)
		for n := start; n <= end; n++ {
			nums = append(nums, n)
		}

		blocks, err := s.blockRange(ctx, nums, true)
		if err != nil {
			return err
		}

		for i, blk := range blocks {
			// 区块里的交易对象不带 blockNumber 字段，块号只能从外层带进来
			blockNumber := nums[i]
			if blk.Number != 0 {
				blockNumber = uint64(blk.Number)
			}
			ts := int64(blk.Timestamp)
			for j := range blk.Transactions {
				if err := s.emitNativeTx(&blk.Transactions[j], native, blockNumber, uint(j), ts, addrs, emit); err != nil {
					return err
				}
			}
		}

		if end >= to {
			return nil
		}
		start = end + 1
	}
}

// emitNativeTx 判定并回调单笔原生转账
func (s *evmSource) emitNativeTx(tx *evmTx, native Token, block uint64, index uint, ts int64, addrs map[string]struct{}, emit func(*Transfer) error) error {
	if tx.To == nil {
		return nil // 合约创建
	}
	toAddr := strings.ToLower(tx.To.Hex())
	if _, ok := addrs[toAddr]; !ok {
		return nil
	}
	if tx.Value == nil {
		return nil
	}
	amount := tx.Value.ToInt()
	if amount.Sign() <= 0 {
		return nil
	}
	// 跳过 gas 钱包的补气转账：那不是用户充值。计入的话，假充值会被归集 worker
	// 把补进去的 gas 扫走，和归集形成死循环。
	if s.gasWallet != "" && strings.EqualFold(tx.From.Hex(), s.gasWallet) {
		return nil
	}
	return emit(&Transfer{
		Chain:     s.cfg.Chain,
		TxHash:    tx.Hash.Hex(),
		Block:     block,
		Kind:      KindNative,
		Index:     index,
		From:      strings.ToLower(tx.From.Hex()),
		To:        toAddr,
		Token:     native,
		Amount:    amount,
		Timestamp: ts,
	})
}

// evmBlock 区块的最小解码结构。
//
// 刻意不用 types.Block：它对整个区块头做严格解码，包括本库根本不读的
// stateRoot/receiptsRoot/transactionsRoot，任何不完全标准的 EVM 链都会整块读不出来。
// types.Transaction 还刻意不带 from，逼调用方从签名反推发送方——链只要不是标准
// ECDSA/EIP-155 签名，反推出来的就是一个看似合法、实际错误的地址，属于静默的数据损坏。
// 节点给的 from 直接采信即可：节点要会撒谎，别的字段一样可以撒谎。
type evmBlock struct {
	Number       hexutil.Uint64 `json:"number"`
	Timestamp    hexutil.Uint64 `json:"timestamp"`
	Transactions []evmTx        `json:"transactions"`
}

type evmTx struct {
	Hash  common.Hash     `json:"hash"`
	From  common.Address  `json:"from"`
	To    *common.Address `json:"to"` // 合约创建时为 null
	Value *hexutil.Big    `json:"value"`
}

// UnmarshalJSON 兼容两种形态：eth_getBlockByNumber(n, true) 给交易对象，
// 而 (n, false) 给的是交易哈希字符串数组——补时间戳那一路用的正是 false。
func (t *evmTx) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &t.Hash)
	}
	type plain evmTx // 避免递归调用本方法
	return json.Unmarshal(data, (*plain)(t))
}

// blockRange 批量取区块。full 为 false 时只要区块头（补时间戳用，省掉交易列表）。
//
// 批量大小是自适应的：节点以「太大」为由拒绝时逐级减半并记下来，之后一直用更小的批。
// 不只是为了兼容小气的公共节点——批量被拒时整批数据都拿不到，重试必须缩批才有意义。
func (s *evmSource) blockRange(ctx context.Context, nums []uint64, full bool) ([]*evmBlock, error) {
	if len(nums) == 0 {
		return nil, nil
	}
	out := make([]*evmBlock, len(nums))

	for start := 0; start < len(nums); {
		size := int(s.blockChunkSize())
		end := min(start+size, len(nums))
		if err := s.blockBatch(ctx, nums[start:end], full, out[start:end]); err != nil {
			if size > 1 && isBatchTooLarge(err) {
				s.shrinkBlockChunk(size)
				continue // 不推进 start：用更小的批重试同一段
			}
			return nil, err
		}
		start = end
	}
	return out, nil
}

// blockBatch 一次批量请求取一段区块
func (s *evmSource) blockBatch(ctx context.Context, nums []uint64, full bool, out []*evmBlock) error {
	elems := make([]rpc.BatchElem, len(nums))
	for i, n := range nums {
		elems[i] = rpc.BatchElem{
			Method: "eth_getBlockByNumber",
			Args:   []interface{}{hexutil.Uint64(n), full},
			Result: &out[i],
		}
	}
	if err := s.pool.batch(ctx, elems); err != nil {
		return err
	}
	for i := range elems {
		if elems[i].Error != nil {
			return fmt.Errorf("获取区块 %d 失败: %w", nums[i], elems[i].Error)
		}
		if out[i] == nil {
			return fmt.Errorf("区块 %d 不存在（节点尚未同步到该高度？）", nums[i])
		}
	}
	return nil
}

// blockChunkSize 当前的批量取块上限
func (s *evmSource) blockChunkSize() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blockChunk
}

// shrinkBlockChunk 批量取块被拒后收窄批大小（只降不升）
func (s *evmSource) shrinkBlockChunk(current int) {
	next := uint64(max(current/2, 1))
	s.mu.Lock()
	if next < s.blockChunk {
		s.blockChunk = next
		s.logf("chainkit: eth_getBlockByNumber 批量上限收窄至 %d 块", next)
	}
	s.mu.Unlock()
}

// ============================================================================
// 余额与提现
// ============================================================================

// Balance 查询余额（最小单位）
// Call 通用只读 eth_call，走的是同一个故障转移池。
//
// 给「调用方自己拼 calldata」的场景用（比如 Multicall3 的批量聚合）——
// 那类调用一次要拉几十上百个值，走池子才能享受端点切换与限流分类。
//
// gas 传 0 表示不指定，由节点自己的默认上限决定；批量聚合要显式给一个，
// 否则大 batch 会被节点的 eth_call gas 上限直接拒掉。
func (s *evmSource) Call(ctx context.Context, to string, data []byte, gas uint64) ([]byte, error) {
	call := map[string]interface{}{
		"to":   common.HexToAddress(to),
		"data": hexutil.Bytes(data),
	}
	if gas > 0 {
		call["gas"] = hexutil.Uint64(gas)
	}
	var raw hexutil.Bytes
	if err := s.pool.call(ctx, &raw, "eth_call", call, "latest"); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *evmSource) Balance(ctx context.Context, addr string, tok *Token) (*big.Int, error) {
	holder := common.HexToAddress(addr)

	if tok.IsNative() {
		var v hexutil.Big
		if err := s.pool.call(ctx, &v, "eth_getBalance", holder, "latest"); err != nil {
			return nil, err
		}
		return (*big.Int)(&v), nil
	}

	var raw hexutil.Bytes
	call := map[string]interface{}{
		"to":   common.HexToAddress(tok.Contract),
		"data": hexutil.Bytes(erc20BalanceOfData(addr)),
	}
	if err := s.pool.call(ctx, &raw, "eth_call", call, "latest"); err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}

// Send 签名并广播一笔转账（legacy / EIP-155 交易）。
//
// 用 legacy 而不是 EIP-1559：BSC、多数 EVM 兼容链与自建节点都还以 legacy 为主，
// 覆盖面更广。手续费与余额预检、并发 nonce 管理不在这里做——那属于上层业务。
func (s *evmSource) Send(ctx context.Context, req *SendRequest) (string, error) {
	if strings.TrimSpace(s.cfg.PrivateKey) == "" {
		return "", errors.New("chainkit: 未配置 PrivateKey，无法提现")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(s.cfg.PrivateKey), "0x"))
	if err != nil {
		return "", fmt.Errorf("chainkit: 私钥格式错误: %w", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)

	to := common.HexToAddress(req.To)
	value := req.Amount
	var data []byte
	if !req.Token.IsNative() {
		// 代币转账是「打给合约、金额为 0、附 transfer 数据」
		to = common.HexToAddress(req.Token.Contract)
		data = erc20TransferData(req.To, req.Amount)
		value = big.NewInt(0)
	}

	nonce, err := s.pendingNonce(ctx, from)
	if err != nil {
		return "", err
	}
	gasPrice, err := s.gasPrice(ctx)
	if err != nil {
		return "", err
	}
	gasLimit, err := s.estimateGas(ctx, from, to, value, data)
	if err != nil {
		return "", err
	}
	gasLimit = uint64(float64(gasLimit) * (1 + s.cfg.gasBuffer))

	tx := types.NewTransaction(nonce, to, value, gasLimit, gasPrice, data)
	signed, err := types.SignTx(tx, types.NewEIP155Signer(new(big.Int).SetUint64(s.pool.chainID())), key)
	if err != nil {
		return "", fmt.Errorf("chainkit: 交易签名失败: %w", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("chainkit: 交易序列化失败: %w", err)
	}

	var hash common.Hash
	if err := s.pool.call(ctx, &hash, "eth_sendRawTransaction", hexutil.Bytes(raw)); err != nil {
		return "", fmt.Errorf("chainkit: 广播交易失败: %w", err)
	}
	return hash.Hex(), nil
}

func (s *evmSource) pendingNonce(ctx context.Context, from common.Address) (uint64, error) {
	var n hexutil.Uint64
	if err := s.pool.call(ctx, &n, "eth_getTransactionCount", from, "pending"); err != nil {
		return 0, fmt.Errorf("chainkit: 获取 nonce 失败: %w", err)
	}
	return uint64(n), nil
}

func (s *evmSource) gasPrice(ctx context.Context) (*big.Int, error) {
	var v hexutil.Big
	if err := s.pool.call(ctx, &v, "eth_gasPrice"); err != nil {
		return nil, fmt.Errorf("chainkit: 获取 gas 价格失败: %w", err)
	}
	return (*big.Int)(&v), nil
}

func (s *evmSource) estimateGas(ctx context.Context, from, to common.Address, value *big.Int, data []byte) (uint64, error) {
	call := map[string]interface{}{
		"from":  from,
		"to":    to,
		"value": (*hexutil.Big)(value),
	}
	if len(data) > 0 {
		call["data"] = hexutil.Bytes(data)
	}
	var n hexutil.Uint64
	if err := s.pool.call(ctx, &n, "eth_estimateGas", call); err != nil {
		return 0, fmt.Errorf("chainkit: gas 估算失败（余额或授权不足？）: %w", err)
	}
	return uint64(n), nil
}

// ============================================================================
// 小工具
// ============================================================================

// sortedKeys 取集合的键并排序，保证请求参数里的地址顺序稳定（便于比对与复现）
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// chunkStrings 按 n 个一组切分
func chunkStrings(in []string, n int) [][]string {
	if n <= 0 {
		n = 1
	}
	var out [][]string
	for start := 0; start < len(in); start += n {
		out = append(out, in[start:min(start+n, len(in))])
	}
	return out
}
