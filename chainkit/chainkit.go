// Package chainkit 提供不依赖数据库的多链 RPC 读写客户端与区块扫描器。
//
// 解决的问题：多个模块都需要「监听链上充值」和「发起链上提现」，各写一份的代价
// 是重复实现区块抓取、日志解析、游标推进、确认数等待、RPC 限流退避——这些与业务
// 无关，却每份都要重新踩一遍坑。
//
// 本库把这条链路收成一个对象：
//
//	c, _ := chainkit.New(chainkit.Config{
//	    Chain:      "bsc",
//	    RPCs:       []string{"https://rpc1", "https://rpc2"}, // 故障转移，非轮询
//	    Tokens:     []chainkit.Token{{Symbol: "BNB"}, {Symbol: "USDT", Contract: "0x55d3...", Decimals: 18}},
//	    Addresses:  func() []string { return myDepositAddresses() }, // 由调用方提供，库不碰 DB
//	    Cursor:     myMongoCursor,                                   // 同上
//	    OnTransfer: func(ctx context.Context, t *chainkit.Transfer) error { return credit(t) },
//	})
//	go c.Watch(ctx)                                   // 充值：阻塞扫描
//	hash, err := c.Send(ctx, &chainkit.SendRequest{...}) // 提现：签名广播
//
// 三条必须先知道的语义：
//
//  1. 看到就回调，**不等确认数**。扫描一路追到链头，链上出现一笔打给关注地址的
//     转账就立刻回调。等待确认、判断这笔充值是否最终成立，是充值业务自己的事——
//     它才有「记一笔 pending 充值、够了再入账」所需要的那张表。库这边只负责
//     「链上出现了什么」，不替调用方保管未确认状态。
//
//     所以回调的语义是「看到」，不是「已确认」：它可能是随后被回滚的交易。
//     真正的入账判据必须由调用方自己攒（见 README 的确认数一节）。
//
//  2. 回调是 at-least-once：OnTransfer 返回 error 时不推进游标，该区间会重扫，
//     之前的转账会重放。调用方必须按 Transfer.Key() 自己去重。
//
//  3. 游标与关注地址都由调用方提供，库不做任何持久化。游标的语义是
//     「已经扫到哪个高度」，不是「哪个高度的充值已经可以入账」。
package chainkit

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// Kind 转账类型。同一笔交易里原生转账与代币转账的序号可能相同，
// 去重键必须带上 Kind，否则会互相覆盖。
type Kind string

const (
	KindNative Kind = "native" // 原生币转账（Index = 块内交易序号）
	KindToken  Kind = "token"  // 代币转账（Index = 日志序号 logIndex）
)

// Token 币种配置。Contract 为空表示链的原生币。
type Token struct {
	Symbol   string
	Contract string
	Decimals int
}

// IsNative 是否原生币
func (t Token) IsNative() bool { return t.Contract == "" }

// String 便于日志输出
func (t Token) String() string {
	if t.IsNative() {
		return t.Symbol
	}
	return t.Symbol + "(" + t.Contract + ")"
}

// Transfer 一笔被识别到的链上转账。金额是最小单位的整数，展示时用 FormatUnits 换算。
type Transfer struct {
	Chain     string
	TxHash    string
	Block     uint64
	Kind      Kind
	Index     uint // KindNative = 块内交易序号；KindToken = 日志序号
	From      string
	To        string // 小写 0x 前缀
	Token     Token
	Amount    *big.Int
	Timestamp int64
}

// Key 去重键：同一笔转账在重扫时会拿到完全相同的 Key，调用方用它做幂等。
func (t *Transfer) Key() string {
	return fmt.Sprintf("%s:%s:%d", t.TxHash, t.Kind, t.Index)
}

// Human 人类可读金额
func (t *Transfer) Human() string {
	return FormatUnits(t.Amount, t.Token.Decimals)
}

// Cursor 扫描游标。库不落库，由调用方提供实现（Mongo / 文件 / 内存）。
// block 的语义是「已经扫到哪个高度」（闭区间，含该块），不是「哪个高度的充值
// 已经可以入账」——入账的判据是调用方自己的确认数，两件事不要混用同一个值。
// 未设置时 ok 返回 false。
type Cursor interface {
	Load(ctx context.Context) (block uint64, ok bool, err error)
	Save(ctx context.Context, block uint64) error
}

// MemoryCursor 进程内游标，仅适合 DEMO 与测试：进程重启后会从 Lookback 重新开始。
type MemoryCursor struct {
	mu    sync.Mutex
	block uint64
	known bool
}

// NewMemoryCursor 创建内存游标
func NewMemoryCursor() *MemoryCursor { return &MemoryCursor{} }

// Load 实现 Cursor
func (c *MemoryCursor) Load(context.Context) (uint64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.block, c.known, nil
}

// Save 实现 Cursor
func (c *MemoryCursor) Save(_ context.Context, block uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.block, c.known = block, true
	return nil
}

// Config 一条链的读写配置。
type Config struct {
	Chain string // 链名标识，写入 Transfer.Chain
	Type  string // 链类型，目前仅支持 "evm"（留空等同 evm）

	// RPCs 按优先级排列的 RPC 端点。它们是故障转移关系而非负载均衡：
	// 健康时只用 RPCs[0]，它失败才切到下一个。轮询多个节点会让不同节点的
	// head 高度互相打架，把游标推得来回抖。
	RPCs []string

	// ChainID 可选。>0 时启动会逐个校验端点，链 ID 不匹配的端点直接剔除——
	// 端点数组里混进测试网地址是静默的数据损坏，必须拦掉。
	// 为 0 时以第一个可连通端点的链 ID 为准。
	ChainID uint64

	// Tokens 要监听的币种。原生币是 Contract 为空的那一项；不配则不扫原生
	// （原生转账只能逐块取交易本地过滤，繁忙链上代价明显）。
	//
	// 这是静态配置。需要运行时增删改（比如新上线一个代币、停掉某个代币的监听）
	// 就用 TokenSource，它每轮扫描前重新读一次，不用重启。
	Tokens []Token

	// TokenSource 动态返回当前要监听的币种，设置后覆盖 Tokens。
	//
	// 和 Addresses 一样每轮扫描前调用一次，所以增删改下一轮就生效：
	//   - 新增：从当前游标往后开始识别该代币的转账（cursor 之前的历史不会补扫）
	//   - 删除：立刻停止识别。之后重新加回来，中间那段也不会补扫
	//   - 改 decimals：只影响之后回调里的 Transfer.Token，已回调的不动
	//
	// 有个反直觉的地方值得留意：把原生币（Contract 为空）加进来会开启原生扫描，
	// 那是逐块取全量交易的贵路径；别拿它来回切。
	TokenSource func() []Token

	// Confirmations 可选的扫描滞后块数。默认 0 = 一路扫到链头、看到就回调，
	// 确认数交给调用方自己跟（见包注释与 README）。
	//
	// 设成 >0 只影响一件事：库不再回调最近 Confirmations 个块里的转账。它**不产生**
	// 「已确认」的保证——只是把浅层重组挡在回调之外，代价是回调延迟。想让入账更稳
	// 应该加在调用方的确认数判断上，而不是靠这个字段。
	Confirmations uint64

	Batch       uint64        // 单轮推进的最大区块数，也是代币日志的单次查询区间上限。0 → 1000
	NativeBatch uint64        // 原生币单次批量取块的块数。0 → 20
	Interval    time.Duration // 追平后的空转间隔。0 → 3s
	Lookback    uint64        // 首次启动回溯多少块。0 → 120
	StartBlock  uint64        // >0 时覆盖 Lookback，强制从该高度起扫
	MaxCatchup  uint64        // 落后超过这个块数就快进（区间内充值不回调）。0 → 10000
	GasWallet   string        // 可选。该地址发出的原生转账会被跳过（补气转账不是充值）

	// Addresses 返回当前需要关注的收款地址，每轮扫描前调用一次，新增的地址即时生效。
	// 库不缓存、不落库。返回空列表等价于「暂无关注地址」，不会报错。
	Addresses func() []string

	// OnTransfer 发现一笔打给关注地址的转账就回调——不等确认数，所以它可能是
	// 随后被回滚的交易，入账前请自行确认（见 Confirmations）。
	// 返回 error 会阻止游标推进，该区间下一轮重扫（at-least-once），用 Transfer.Key() 去重。
	OnTransfer func(ctx context.Context, t *Transfer) error

	// Cursor 游标存储。留空则用 MemoryCursor（进程重启即丢，仅适合 DEMO）。
	Cursor Cursor

	PrivateKey string  // 提现用的私钥（hex，可带 0x）。只读用途可以留空
	GasBuffer  float64 // gas 估算的冗余系数。0 → 0.1

	// Logf 日志输出，留空则打到标准错误。库刻意不依赖 common/logger：
	// 那需要 config.yaml 与全局初始化，库不该强迫调用方具备这些。
	Logf func(format string, args ...any)

	// 运行时有效值，由 New 填好
	batch       uint64
	nativeBatch uint64
	interval    time.Duration
	lookback    uint64
	maxCatchup  uint64
	gasBuffer   float64
	logf        func(format string, args ...any)
}

// Chain 一条链的读写门面。并发安全：Watch 只应有一个实例在跑，
// Send/Balance 可以与它并发调用。
type Chain struct {
	cfg *Config
	src source

	// readOnly 由 NewClient 置位：只读实例没有 OnTransfer/Addresses，不允许 Watch。
	readOnly bool

	// testBackoff 仅测试使用：注入确定性的退避时长，避免单测真的等秒级退避。
	testBackoff func(fails int) time.Duration
}

// New 建连并按 cfg 校验。任何 RPC 端点都连不上时返回错误（配置错误要早失败）。
func New(cfg Config) (*Chain, error) {
	if len(cfg.RPCs) == 0 {
		return nil, errors.New("chainkit: 至少要配一个 RPC 端点")
	}
	if len(cfg.Tokens) == 0 && cfg.TokenSource == nil {
		return nil, errors.New("chainkit: 至少要配一个币种（Tokens 或 TokenSource）")
	}
	if cfg.OnTransfer == nil {
		return nil, errors.New("chainkit: OnTransfer 不能为空")
	}
	if cfg.Addresses == nil {
		// 关注地址是监听的全部依据：既决定报告谁，也是代币日志在节点侧的主题过滤条件。
		// 没有它，扫到的每一笔转账都无从判断是不是我们的。
		return nil, errors.New("chainkit: Addresses 不能为空")
	}
	c, err := buildChain(&cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// NewClient 建一个只读客户端，不做地址监听：Balance/Head/ChainID/Confirmations 可用，
// Watch 会明确报错。
//
// 给「只查余额」这类调用方用。New 强制要求 OnTransfer/Addresses 是因为扫描没有它们
// 无从判断哪笔转账与我们有关；但只读数余额既不需要关注地址、也不回调任何东西，
// 逼这类调用方传两个假函数是让它们对着库的类型撒谎。所以这里跳过那两项检查，
// 另外用 readOnly 标记把「不能 Watch」表达成运行时的明确错误，而不是 nil 函数 panic。
func NewClient(cfg Config) (*Chain, error) {
	if len(cfg.RPCs) == 0 {
		return nil, errors.New("chainkit: 至少要配一个 RPC 端点")
	}
	c, err := buildChain(&cfg)
	if err != nil {
		return nil, err
	}
	c.readOnly = true
	return c, nil
}

// buildChain 填默认值、建数据源
func buildChain(cfg *Config) (*Chain, error) {
	applyDefaults(cfg)
	if cfg.Cursor == nil {
		cfg.logf("chainkit: 未提供 Cursor，使用内存游标（进程重启后从 Lookback 重新开始）")
		cfg.Cursor = NewMemoryCursor()
	}

	src, err := newSource(cfg)
	if err != nil {
		return nil, err
	}
	return &Chain{cfg: cfg, src: src}, nil
}

// applyDefaults 填充运行时默认值
func applyDefaults(cfg *Config) {
	if cfg.Type == "" {
		cfg.Type = "evm"
	}
	cfg.batch = orDefault(cfg.Batch, 1000)
	cfg.nativeBatch = orDefault(cfg.NativeBatch, 20)
	cfg.maxCatchup = orDefault(cfg.MaxCatchup, 10000)
	if cfg.lookback = orDefault(cfg.Lookback, 120); cfg.StartBlock > 0 {
		cfg.lookback = 0
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 3 * time.Second
	}
	cfg.interval = cfg.Interval
	if cfg.GasBuffer <= 0 {
		cfg.GasBuffer = 0.1
	}
	cfg.gasBuffer = cfg.GasBuffer
	if cfg.Logf != nil {
		cfg.logf = cfg.Logf
	} else {
		cfg.logf = defaultLogf
	}
}

func orDefault[T ~int | ~uint64](v, def T) T {
	if v == 0 {
		return def
	}
	return v
}

// Watch 阻塞运行扫描循环，直到 ctx 取消或遇到不可恢复的错误。
// 想在后台跑就 go c.Watch(ctx)。
//
// 只读实例（NewClient 建的）调这里会立刻得到明确错误，而不是扫到一半才发现没有回调。
func (c *Chain) Watch(ctx context.Context) error {
	if c.readOnly {
		return errors.New("chainkit: 只读客户端不能 Watch（请用 New 并提供 OnTransfer/Addresses）")
	}
	return c.watch(ctx)
}

// Call 发一次只读 eth_call（调用方自己拼 calldata），返回原始返回数据。
// gas 传 0 表示不指定。走的是与 Balance 相同的多端点故障转移池。
func (c *Chain) Call(ctx context.Context, to string, data []byte, gas uint64) ([]byte, error) {
	return c.src.Call(ctx, to, data, gas)
}

// Send 签名并广播一笔转账，返回交易哈希。req.Amount 是最小单位的整数。
func (c *Chain) Send(ctx context.Context, req *SendRequest) (string, error) {
	if req == nil {
		return "", errors.New("chainkit: SendRequest 为空")
	}
	if req.To == "" {
		return "", errors.New("chainkit: 缺少收款地址")
	}
	if req.Amount == nil || req.Amount.Sign() <= 0 {
		return "", errors.New("chainkit: 转账金额必须大于 0")
	}
	return c.src.Send(ctx, req)
}

// SendRequest 提现请求
type SendRequest struct {
	To     string
	Amount *big.Int // 最小单位，人类可读金额先用 ParseUnits 换算
	Token  Token    // Contract 为空即原生币
}

// Balance 查询地址的余额（最小单位）。token 传 nil 时按第一个原生币查；
// 没有配置原生币又传 nil 会返回错误。
func (c *Chain) Balance(ctx context.Context, addr string, token *Token) (*big.Int, error) {
	t := token
	if t == nil {
		for i := range c.cfg.Tokens {
			if c.cfg.Tokens[i].IsNative() {
				t = &c.cfg.Tokens[i]
				break
			}
		}
	}
	if t == nil {
		return nil, errors.New("chainkit: 未指定币种，且配置里没有原生币")
	}
	return c.src.Balance(ctx, addr, t)
}

// ChainID 链 ID
func (c *Chain) ChainID(ctx context.Context) (uint64, error) { return c.src.ChainID(ctx) }

// Head 当前链头高度
func (c *Chain) Head(ctx context.Context) (uint64, error) { return c.src.Head(ctx) }

// Confirmations 某个高度之上又出了多少个块（含该块本身，所以刚出的块是 1）。
// 链头还没到这个高度时返回 0。
//
// 库看到就回调、不等确认数，所以「够了再入账」这一步要靠调用方自己补。
// Transfer.Block 加上这个函数就够判断了：
//
//	if n, _ := c.Confirmations(ctx, t.Block); n >= required {
//	    credit(t.Key())
//	}
func (c *Chain) Confirmations(ctx context.Context, block uint64) (uint64, error) {
	head, err := c.src.Head(ctx)
	if err != nil {
		return 0, err
	}
	if head < block {
		return 0, nil
	}
	return head - block + 1, nil
}

// backoff 连续失败 fails 次后的等待时长。
func (c *Chain) backoff(fails int) time.Duration {
	if c.testBackoff != nil {
		return c.testBackoff(fails)
	}
	return backoffFor(fails)
}

// Close 释放 RPC 连接
func (c *Chain) Close() error { return c.src.Close() }
