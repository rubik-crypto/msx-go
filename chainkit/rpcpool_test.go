package chainkit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// newTestPool 建一个静默日志的端点池
func newTestPool(t *testing.T, urls []string, wantChainID uint64) (*rpcPool, error) {
	t.Helper()
	return newRPCPool(context.Background(), urls, wantChainID, func(string, ...any) {})
}

// TestPoolSticksToPreferred 健康时必须只用首选端点。
// 轮询多个端点会让每次请求落到高度不同的节点上，游标被推得来回抖。
func TestPoolSticksToPreferred(t *testing.T) {
	first := newFakeNode(t, 56)
	second := newFakeNode(t, 56)

	pool, err := newTestPool(t, []string{first.URL(), second.URL()}, 0)
	if err != nil {
		t.Fatalf("建池失败: %v", err)
	}
	defer pool.close()

	for i := 0; i < 3; i++ {
		var n hexutil.Uint64
		if err := pool.call(context.Background(), &n, "eth_blockNumber"); err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i, err)
		}
	}

	if got := len(first.callsOf("eth_blockNumber")); got != 3 {
		t.Errorf("首选端点应收到 3 次调用，实际 %d", got)
	}
	if got := len(second.callsOf("eth_blockNumber")); got != 0 {
		t.Errorf("备用端点不该被调用，实际 %d 次", got)
	}
	if !pool.preferred() {
		t.Error("健康时应当停在首选端点")
	}
}

// TestPoolRotatesOnTransportError 传输层错误要换端点重试
func TestPoolRotatesOnTransportError(t *testing.T) {
	first := newFakeNode(t, 56)
	second := newFakeNode(t, 56)

	pool, err := newTestPool(t, []string{first.URL(), second.URL()}, 0)
	if err != nil {
		t.Fatalf("建池失败: %v", err)
	}
	defer pool.close()

	first.setHTTPStatus(http.StatusInternalServerError)

	var n hexutil.Uint64
	if err := pool.call(context.Background(), &n, "eth_blockNumber"); err != nil {
		t.Fatalf("应当由备用端点兜住，实际失败: %v", err)
	}
	if got := len(second.callsOf("eth_blockNumber")); got != 1 {
		t.Errorf("备用端点应收到 1 次调用，实际 %d", got)
	}
	if pool.preferred() {
		t.Error("应当已经切离首选端点")
	}
}

// TestPoolDoesNotRotateOnSemanticError 语义错误换端点也没用，不该轮换——
// 轮换只会白白多打一次请求，还掩盖了真正的原因
func TestPoolDoesNotRotateOnSemanticError(t *testing.T) {
	first := newFakeNode(t, 56)
	second := newFakeNode(t, 56)
	first.handle = func(*fakeNode, string, []json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}

	pool, err := newTestPool(t, []string{first.URL(), second.URL()}, 0)
	if err != nil {
		t.Fatalf("建池失败: %v", err)
	}
	defer pool.close()

	var out any
	err = pool.call(context.Background(), &out, "eth_getLogs", map[string]any{})
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if !IsMethodMissing(err) {
		t.Errorf("错误应被识别为方法不支持: %v", err)
	}
	if got := len(second.callsOf("eth_getLogs")); got != 0 {
		t.Errorf("语义错误不该轮换到备用端点，实际调用了 %d 次", got)
	}
	if !pool.preferred() {
		t.Error("语义错误后应仍停在首选端点")
	}
}

// TestPoolDropsMismatchedChainID 端点数组里混进别的链是静默的数据损坏，必须剔除
func TestPoolDropsMismatchedChainID(t *testing.T) {
	wrong := newFakeNode(t, 1)
	right := newFakeNode(t, 56)

	pool, err := newTestPool(t, []string{wrong.URL(), right.URL()}, 56)
	if err != nil {
		t.Fatalf("建池失败: %v", err)
	}
	defer pool.close()

	if got := len(pool.eps); got != 1 {
		t.Fatalf("应当只保留 1 个端点，实际 %d", got)
	}
	if pool.eps[0].chainID != 56 {
		t.Errorf("保留下来的端点链 ID 应为 56，实际 %d", pool.eps[0].chainID)
	}
	if got := len(wrong.callsOf("eth_blockNumber")); got != 0 {
		t.Errorf("被剔除的端点不该再被调用，实际 %d 次", got)
	}
}

// TestPoolErrorsWhenAllEndpointsUnusable 配置全错要早失败
func TestPoolErrorsWhenAllEndpointsUnusable(t *testing.T) {
	broken := newFakeNode(t, 56)
	broken.setHTTPStatus(http.StatusBadGateway)

	if _, err := newTestPool(t, []string{broken.URL()}, 0); err == nil {
		t.Fatal("全部端点不可用时应当报错")
	}
	// 混入一个链 ID 不符的端点，也不该凑数
	mismatch := newFakeNode(t, 1)
	if _, err := newTestPool(t, []string{mismatch.URL()}, 56); err == nil {
		t.Fatal("链 ID 全部不符时应当报错")
	}
}

// TestPoolProbesPreferred 切走之后要能回归首选端点
func TestPoolProbesPreferred(t *testing.T) {
	first := newFakeNode(t, 56)
	second := newFakeNode(t, 56)

	pool, err := newTestPool(t, []string{first.URL(), second.URL()}, 0)
	if err != nil {
		t.Fatalf("建池失败: %v", err)
	}
	defer pool.close()

	first.setHTTPStatus(http.StatusInternalServerError)
	var n hexutil.Uint64
	if err := pool.call(context.Background(), &n, "eth_blockNumber"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if pool.preferred() {
		t.Fatal("此时应在备用端点")
	}

	// 首选端点恢复。冷却期没到本来就不该探测，这里直接把冷却抹掉以缩短测试。
	first.setHTTPStatus(0)
	pool.mu.Lock()
	pool.eps[0].downUntil = time.Time{}
	pool.mu.Unlock()

	pool.probePreferred(context.Background())
	if !pool.preferred() {
		t.Error("首选端点恢复后应当切回去")
	}

	// 冷却期内不该频繁探测
	pool.mu.Lock()
	pool.active = 1
	pool.eps[0].downUntil = time.Now().Add(time.Minute)
	before := len(first.callsOf("eth_blockNumber"))
	pool.mu.Unlock()

	pool.probePreferred(context.Background())
	if got := len(first.callsOf("eth_blockNumber")); got != before {
		t.Errorf("冷却期内不该探测首选端点，多打了 %d 次", got-before)
	}
}

func TestRedactURLHidesKeys(t *testing.T) {
	got := redactURL("https://bsc.example.com/v2/super-secret-key")
	if strings.Contains(got, "super-secret-key") {
		t.Fatalf("密钥泄漏到日志里了: %s", got)
	}
	if got != "https://bsc.example.com/***" {
		t.Errorf("脱敏结果不对: %s", got)
	}
	if got := redactURL("不是个地址"); got != "***" {
		t.Errorf("解析不了的地址应整体脱敏，实际 %s", got)
	}
}

// TestClassify 错误分类直接决定了「该换端点还是该缩请求」，判错会放大问题
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want errClass
	}{
		{"HTTP 429", rpc.HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"}, classThrottle},
		{"HTTP 413", rpc.HTTPError{StatusCode: http.StatusRequestEntityTooLarge, Status: "413"}, classRequestTooLarge},
		{"HTTP 400 表示请求过大", rpc.HTTPError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: []byte("Bad Request")}, classRequestTooLarge},
		{"HTTP 502", rpc.HTTPError{StatusCode: http.StatusBadGateway, Status: "502"}, classTransport},
		{"文本限流", errors.New("rate limit exceeded"), classThrottle},
		{"限流带 limit 字样也不能误判成区间", errors.New("query limit exceeded, slow down"), classThrottle},
		{"区间过大", errors.New("query returned more than 10000 results"), classRangeLimit},
		{"区块区间", errors.New("block range is too large"), classRangeLimit},
		{"方法缺失文本", errors.New("the method eth_getLogs does not exist/is not available"), classMethodMissing},
		{"方法缺失 JSON-RPC 文本变体", errors.New("method not found"), classMethodMissing},
		{"连接被拒", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), classTransport},
		{"EOF", errors.New("unexpected EOF"), classTransport},
		{"无错误", nil, classOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.err); got != c.want {
				t.Errorf("classify(%v) = %d, 期望 %d", c.err, got, c.want)
			}
		})
	}
}

// “query limit exceeded” 这类文本同时含 limit 与 exceeded，必须是限流而不是区间
func TestClassifyThrottleBeforeRange(t *testing.T) {
	err := errors.New("429 Too Many Requests: quota exceeded, limit exceeded")
	if !IsThrottled(err) {
		t.Error("应判定为限流")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for n := 1; n <= 8; n++ {
		got := backoffFor(n)
		if n == 1 {
			prev = got
			continue
		}
		if got < time.Second*3/4 || got > 60*time.Second*6/5 {
			t.Errorf("backoffFor(%d) = %s，超出合理区间", n, got)
		}
		if n <= 6 && got < prev {
			t.Errorf("backoffFor(%d) = %s 不应小于前一次的 %s", n, got, prev)
		}
		prev = got
	}
	if got := backoffFor(100); got > 60*time.Second*6/5 {
		t.Errorf("退避上限被突破: %s", got)
	}
}

// 需要并发调用时不出现数据竞争（配合 -race 跑）
func TestPoolConcurrentCalls(t *testing.T) {
	node := newFakeNode(t, 56)
	pool, err := newTestPool(t, []string{node.URL()}, 0)
	if err != nil {
		t.Fatalf("建池失败: %v", err)
	}
	defer pool.close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n hexutil.Uint64
			_ = pool.call(context.Background(), &n, "eth_blockNumber")
		}()
	}
	wg.Wait()
}
