package chainkit

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"
)

// fakeSource 只实现 source 接口，不碰网络——扫描循环的正确性不该依赖 RPC。
type fakeSource struct {
	mu        sync.Mutex
	head      uint64
	headErr   error
	scanErr   error
	preferred bool
	chainID   uint64
	ranges    [][2]uint64
	// transferAt 若不为 nil，命中该块时产生一笔转账
	transferAt map[uint64]*Transfer
}

func newFakeSource(head uint64) *fakeSource {
	return &fakeSource{head: head, preferred: true, chainID: 56}
}

func (f *fakeSource) Head(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, f.headErr
}

func (f *fakeSource) ChainID(context.Context) (uint64, error) { return f.chainID, nil }

func (f *fakeSource) ScanRange(_ context.Context, from, to uint64, emit func(*Transfer) error) error {
	f.mu.Lock()
	f.ranges = append(f.ranges, [2]uint64{from, to})
	err := f.scanErr
	transfers := make([]*Transfer, 0, 1)
	for n := from; n <= to; n++ {
		if tr, ok := f.transferAt[n]; ok {
			transfers = append(transfers, tr)
		}
	}
	f.mu.Unlock()

	if err != nil {
		return err
	}
	for _, tr := range transfers {
		if err := emit(tr); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSource) Balance(context.Context, string, *Token) (*big.Int, error) {
	return big.NewInt(0), nil
}

func (f *fakeSource) Send(context.Context, *SendRequest) (string, error) { return "0x0", nil }

func (f *fakeSource) Call(context.Context, string, []byte, uint64) ([]byte, error) { return nil, nil }

func (f *fakeSource) Probe(context.Context) {}

func (f *fakeSource) Preferred() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preferred
}

func (f *fakeSource) Close() error { return nil }

func (f *fakeSource) scanned() [][2]uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]uint64(nil), f.ranges...)
}

func (f *fakeSource) setPreferred(v bool) {
	f.mu.Lock()
	f.preferred = v
	f.mu.Unlock()
}

// recCursor 记录全部写入的游标
type recCursor struct {
	mu      sync.Mutex
	block   uint64
	known   bool
	written []uint64
}

func newRecCursor(block uint64, known bool) *recCursor {
	return &recCursor{block: block, known: known}
}

func (c *recCursor) Load(context.Context) (uint64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.block, c.known, nil
}

func (c *recCursor) Save(_ context.Context, block uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.block, c.known = block, true
	c.written = append(c.written, block)
	return nil
}

func (c *recCursor) value() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.block
}

func (c *recCursor) saves() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint64(nil), c.written...)
}

// newTestChain 直接把假数据源装进 Chain，绕开建连
func newTestChain(t *testing.T, cfg Config, src source, cur Cursor) *Chain {
	t.Helper()
	if cfg.Chain == "" {
		cfg.Chain = "test"
	}
	cfg.Logf = func(string, ...any) {}
	applyDefaults(&cfg)
	cfg.Cursor = cur
	if cfg.OnTransfer == nil {
		cfg.OnTransfer = func(context.Context, *Transfer) error { return nil }
	}
	return &Chain{cfg: &cfg, src: src}
}

// runWatch 后台跑 Watch，等到 cond 成立（或超时）后取消
func runWatch(t *testing.T, c *Chain, timeout time.Duration, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Watch(ctx)
	}()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			cancel()
			<-done
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("等待条件超时")
}

// TestWatchScansToHeadByDefault 默认不等确认数：一路扫到链头，看到转账就回调
func TestWatchScansToHeadByDefault(t *testing.T) {
	src := newFakeSource(100)
	cur := newRecCursor(0, false)
	c := newTestChain(t, Config{
		StartBlock: 1,
		Batch:      1000,
		Interval:   time.Millisecond,
	}, src, cur)

	runWatch(t, c, 2*time.Second, func() bool { return len(src.scanned()) > 0 })

	ranges := src.scanned()
	if ranges[0] != [2]uint64{1, 100} {
		t.Errorf("默认应当扫到链头 [1,100]，实际 %v", ranges[0])
	}
	if got := cur.value(); got != 100 {
		t.Errorf("游标应推进到链头 100，实际 %d", got)
	}
}

// TestWatchHonorsOptionalScanLag Confirmations 仍可作为可选的扫描滞后：
// 把最近几块挡在回调之外（滤浅层重组噪音），但它不构成任何「已确认」保证
func TestWatchHonorsOptionalScanLag(t *testing.T) {
	src := newFakeSource(100)
	cur := newRecCursor(0, false)
	c := newTestChain(t, Config{
		Confirmations: 10,
		StartBlock:    1,
		Batch:         1000,
		Interval:      time.Millisecond,
	}, src, cur)

	runWatch(t, c, 2*time.Second, func() bool { return len(src.scanned()) > 0 })

	ranges := src.scanned()
	if len(ranges) == 0 {
		t.Fatal("应当至少扫过一次")
	}
	if ranges[0] != [2]uint64{1, 90} {
		t.Errorf("给了滞后 10 时首个区间应为 [1,90]，实际 %v", ranges[0])
	}
	if got := cur.value(); got != 90 {
		t.Errorf("游标应推进到 90，实际 %d", got)
	}
	for _, r := range ranges {
		if r[1] > 90 {
			t.Errorf("扫描高度越过了滞后上限: %v", r)
		}
	}
}

// TestWatchConfirmationsExceedHead 链头还没到确认数时不该下溢或空扫
func TestWatchConfirmationsExceedHead(t *testing.T) {
	src := newFakeSource(5)
	cur := newRecCursor(0, false)
	c := newTestChain(t, Config{
		Confirmations: 100,
		StartBlock:    1,
		Interval:      time.Millisecond,
	}, src, cur)

	// 等几轮空转，确认没有扫描发生也没有 panic
	time.Sleep(60 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Watch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("应当返回 ctx 取消，实际 %v", err)
	}
	if got := src.scanned(); len(got) != 0 {
		t.Errorf("高度不足确认数时不该扫描，实际扫了 %v", got)
	}
}

// TestWatchScansInBatches 单轮推进受 Batch 限制，避免一次扫太大块区间
func TestWatchScansInBatches(t *testing.T) {
	src := newFakeSource(50)
	cur := newRecCursor(0, false)
	c := newTestChain(t, Config{
		StartBlock: 1,
		Batch:      10,
		Interval:   time.Millisecond,
	}, src, cur)

	runWatch(t, c, 2*time.Second, func() bool { return cur.value() >= 50 })

	want := [][2]uint64{{1, 10}, {11, 20}, {21, 30}, {31, 40}, {41, 50}}
	got := src.scanned()
	if len(got) < len(want) {
		t.Fatalf("应至少扫 %d 段，实际 %v", len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 段应为 %v，实际 %v", i+1, w, got[i])
		}
	}
}

// TestWatchCursorNotAdvancedOnCallbackError 回调失败时游标绝不能前进，
// 否则这块就被跳过了——副作用没做成的转账会永久丢失
func TestWatchCursorNotAdvancedOnCallbackError(t *testing.T) {
	src := newFakeSource(20)
	src.transferAt = map[uint64]*Transfer{
		5: {Chain: "test", TxHash: "0xaa", Block: 5, Kind: KindToken, Index: 0, Token: Token{Symbol: "USDT", Decimals: 18}, Amount: big.NewInt(1)},
	}
	cur := newRecCursor(0, false)
	sentinel := errors.New("入账失败")
	c := newTestChain(t, Config{
		StartBlock: 1,
		Interval:   time.Millisecond,
		OnTransfer: func(context.Context, *Transfer) error { return sentinel },
	}, src, cur)
	c.testBackoff = func(int) time.Duration { return 5 * time.Millisecond }

	// 等两次扫描尝试：第一次失败之后必须重试，而不是把这块跳过去
	runWatch(t, c, 2*time.Second, func() bool { return len(src.scanned()) >= 2 })

	if got := cur.value(); got != 0 {
		t.Errorf("回调失败时游标不该前进，实际 %d", got)
	}
}

// TestWatchAdvancesAfterSuccessfulCallback 回调成功则游标推进，且只推一次
func TestWatchAdvancesAfterSuccessfulCallback(t *testing.T) {
	src := newFakeSource(20)
	src.transferAt = map[uint64]*Transfer{
		5: {Chain: "test", TxHash: "0xaa", Block: 5, Kind: KindToken, Index: 0, Token: Token{Symbol: "USDT", Decimals: 18}, Amount: big.NewInt(1)},
	}
	cur := newRecCursor(0, false)

	var mu sync.Mutex
	var seen []string
	c := newTestChain(t, Config{
		StartBlock: 1,
		Batch:      5,
		Interval:   time.Millisecond,
		OnTransfer: func(_ context.Context, tr *Transfer) error {
			mu.Lock()
			seen = append(seen, tr.Key())
			mu.Unlock()
			return nil
		},
	}, src, cur)

	runWatch(t, c, 2*time.Second, func() bool { return cur.value() >= 20 })

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("同一笔转账应只回调一次，实际 %d 次: %v", len(seen), seen)
	}
}

// TestWatchRewindsOnDeepRollbackAtPreferredNode 首选节点报出比游标还低的链头，
// 只能解释为链回滚了那么深——不回退就会永远漏掉替换块里的充值
func TestWatchRewindsOnDeepRollbackAtPreferredNode(t *testing.T) {
	src := newFakeSource(500)
	cur := newRecCursor(1000, true)
	c := newTestChain(t, Config{
		Confirmations: 10,
		Lookback:      100,
		Batch:         100,
		Interval:      time.Millisecond,
	}, src, cur)

	// 回退后的游标 = (500-10) - 100 = 390
	runWatch(t, c, 2*time.Second, func() bool { return len(src.scanned()) > 0 })

	rewound := false
	for _, v := range cur.saves() {
		if v == 390 {
			rewound = true
		}
	}
	if !rewound {
		t.Errorf("应当写入过回退后的游标 390，实际写入序列 %v", cur.saves())
	}
	ranges := src.scanned()
	if ranges[0][0] != 391 {
		t.Errorf("回退后应从 391 重新扫起，实际 %v", ranges)
	}
}

// TestWatchDoesNotRewindOnStandbyNode 备用节点落后不是回滚。
// 这里回退游标会把已回调的区间整段重放，什么都没修好。
func TestWatchDoesNotRewindOnStandbyNode(t *testing.T) {
	src := newFakeSource(500)
	src.setPreferred(false)
	cur := newRecCursor(1000, true)
	c := newTestChain(t, Config{
		Confirmations: 10,
		Interval:      time.Millisecond,
	}, src, cur)

	time.Sleep(80 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Watch(ctx)

	if got := cur.value(); got != 1000 {
		t.Errorf("备用节点落后时游标不该被改动，实际 %d", got)
	}
	if got := src.scanned(); len(got) != 0 {
		t.Errorf("链头低于游标时不该扫描，实际 %v", got)
	}
}

// TestWatchFastForwardsWhenTooFarBehind 落后太多就快进，否则长期停机后永远追不上
func TestWatchFastForwardsWhenTooFarBehind(t *testing.T) {
	src := newFakeSource(100_000)
	cur := newRecCursor(1000, true)
	c := newTestChain(t, Config{
		MaxCatchup: 500,
		Batch:      100,
		Interval:   time.Millisecond,
	}, src, cur)

	// 快进到 100000 - 500 = 99500
	runWatch(t, c, 2*time.Second, func() bool { return cur.value() > 1000 })

	ranges := src.scanned()
	if len(ranges) == 0 || ranges[0][0] != 99_501 {
		t.Errorf("快进后应从 99501 扫起，实际 %v", ranges)
	}
}

// TestWatchFirstStartUsesLookback 首次启动要回溯 Lookback，而不是直接跳到链头——
// 直接跳到链头会漏掉停机期间到账的充值
func TestWatchFirstStartUsesLookback(t *testing.T) {
	src := newFakeSource(1000)
	cur := newRecCursor(0, false)
	c := newTestChain(t, Config{
		Lookback: 50,
		Batch:    100,
		Interval: time.Millisecond,
	}, src, cur)

	runWatch(t, c, 2*time.Second, func() bool { return len(src.scanned()) > 0 })

	ranges := src.scanned()
	if ranges[0][0] != 951 {
		t.Errorf("首次扫描应从 1000-50+1=951 开始，实际 %v", ranges[0])
	}
}

// TestWatchRetriesHeadErrors 取链头失败要退避重试而不是退出
func TestWatchRetriesHeadErrors(t *testing.T) {
	src := newFakeSource(0)
	src.headErr = errors.New("connection refused")
	cur := newRecCursor(0, true)
	c := newTestChain(t, Config{Interval: time.Millisecond}, src, cur)
	c.testBackoff = func(int) time.Duration { return 5 * time.Millisecond }

	// 失败期间要持续重试而不是退出；链头恢复后应继续推进
	go func() {
		time.Sleep(80 * time.Millisecond)
		src.mu.Lock()
		src.headErr = nil
		src.head = 30
		src.mu.Unlock()
	}()

	runWatch(t, c, 2*time.Second, func() bool { return cur.value() >= 30 })
}

// TestChainConfirmations 库不等确认数，这个函数是给调用方补上「够了再入账」那一步用的
func TestChainConfirmations(t *testing.T) {
	src := newFakeSource(100)
	c := newTestChain(t, Config{}, src, newRecCursor(0, false))

	cases := []struct {
		block uint64
		want  uint64
	}{
		{100, 1}, // 链头当前的块，算 1 个确认
		{99, 2},
		{1, 100},
		{101, 0}, // 链头还没到这个高度
	}
	for _, tc := range cases {
		got, err := c.Confirmations(context.Background(), tc.block)
		if err != nil {
			t.Fatalf("Confirmations(%d) 报错: %v", tc.block, err)
		}
		if got != tc.want {
			t.Errorf("Confirmations(%d) = %d，期望 %d", tc.block, got, tc.want)
		}
	}
}

// TestWatchScanBackoffEscalatesIndependentlyOfHead 扫描连续失败时退避必须递增。
// 链头查询成功不该把扫描的失败计数清零——否则一个持续拒绝扫描的节点会被
// 以固定 1 秒的间隔一直敲下去。
func TestWatchScanBackoffEscalatesIndependentlyOfHead(t *testing.T) {
	src := newFakeSource(50)
	src.scanErr = errors.New("429 Too Many Requests") // 扫描一直失败，链头一直正常
	cur := newRecCursor(0, false)
	c := newTestChain(t, Config{StartBlock: 1, Interval: time.Millisecond}, src, cur)

	var mu sync.Mutex
	var waits []int
	c.testBackoff = func(fails int) time.Duration {
		mu.Lock()
		waits = append(waits, fails)
		mu.Unlock()
		return 5 * time.Millisecond
	}

	runWatch(t, c, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(waits) >= 3
	})

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3}
	if len(waits) < 3 || waits[0] != want[0] || waits[1] != want[1] || waits[2] != want[2] {
		t.Errorf("连续失败次数应递增为 %v，实际 %v", want, waits)
	}
	if got := cur.value(); got != 0 {
		t.Errorf("扫描一直失败，游标不该前进，实际 %d", got)
	}
}

// TestWatchReturnsOnContextCancel 上下文取消要干净退出
func TestWatchReturnsOnContextCancel(t *testing.T) {
	src := newFakeSource(100)
	cur := newRecCursor(100, true) // 游标已在链头，进空转
	c := newTestChain(t, Config{Interval: time.Millisecond}, src, cur)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Watch(ctx) }()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("应当返回 context.Canceled，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 Watch 没有退出")
	}
}
