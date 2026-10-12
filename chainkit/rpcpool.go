package chainkit

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// rpcTimeout 单个 HTTP 请求的兜底超时。调用方的 ctx 通常没有 deadline，
// 没有这一层的话一次挂死的连接会把扫描循环永远卡住。
const rpcTimeout = 60 * time.Second

// endpoint 一个 RPC 端点及其健康状态
type endpoint struct {
	url       string
	client    *rpc.Client
	chainID   uint64
	fails     int       // 连续失败次数，成功后归零
	downUntil time.Time // 冷却截止时间
}

// rpcPool 多端点故障转移池。
//
// 它是故障转移而不是负载均衡：同一时刻只用 eps[active] 一个端点，它出问题才切走。
// 轮询多个端点会让每次请求落到不同高度的节点上，head 忽高忽低，游标被推得来回抖；
// 更糟的是拿到一个偏矮的 head 会让扫描「倒退」，而这在无数据库的设计里无法区分于重组。
type rpcPool struct {
	mu     sync.Mutex
	eps    []*endpoint
	active int
	wantID uint64
	logf   func(format string, args ...any)
}

// newRPCPool 逐个连接并校验端点，返回可用的端点池。
// 连不上、报不出 chainId、或 chainId 与预期不符的端点会被剔除；
// 一个都不剩时返回错误（配置全错要早失败，而不是每轮扫描都失败）。
func newRPCPool(ctx context.Context, urls []string, wantChainID uint64, logf func(string, ...any)) (*rpcPool, error) {
	p := &rpcPool{wantID: wantChainID, logf: logf}
	hc := &http.Client{Timeout: rpcTimeout}

	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		ep, err := dialEndpoint(ctx, raw, hc)
		if err != nil {
			logf("chainkit: 跳过 RPC 端点 %s: %v", redactURL(raw), err)
			continue
		}
		if p.wantID == 0 {
			p.wantID = ep.chainID // 第一个可用端点定义基准
		}
		if ep.chainID != p.wantID {
			logf("chainkit: 剔除 RPC 端点 %s：链 ID %d 与预期的 %d 不符（混入测试网/错链端点是静默的数据损坏）",
				redactURL(raw), ep.chainID, p.wantID)
			ep.client.Close()
			continue
		}
		p.eps = append(p.eps, ep)
	}

	if len(p.eps) == 0 {
		return nil, fmt.Errorf("chainkit: %d 个 RPC 端点没有一个可用", len(urls))
	}
	logf("chainkit: RPC 端点就绪 %d 个（首选 %s，链 ID %d）", len(p.eps), redactURL(p.eps[0].url), p.wantID)
	return p, nil
}

// dialEndpoint 连接单个端点并读取它的链 ID
func dialEndpoint(ctx context.Context, raw string, hc *http.Client) (*endpoint, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client, err := rpc.DialOptions(dialCtx, raw, rpc.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	var id hexutil.Uint64
	if err := client.CallContext(dialCtx, &id, "eth_chainId"); err != nil {
		client.Close()
		return nil, fmt.Errorf("读取链 ID 失败: %w", err)
	}
	return &endpoint{url: raw, client: client, chainID: uint64(id)}, nil
}

// call 发起一次 JSON-RPC 调用，遇到可转移的错误自动切换端点重试。
// 语义类错误（区间过大、方法不支持）不重试，原样返回给上层走回退阶梯。
func (p *rpcPool) call(ctx context.Context, out any, method string, args ...any) error {
	var lastErr error
	for attempt := 0; attempt < len(p.eps); attempt++ {
		ep := p.current()
		err := ep.client.CallContext(ctx, out, method, args...)
		if err == nil {
			p.markOK(ep)
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return err
		}
		if !p.rotateOn(ep, err) {
			return err
		}
	}
	return lastErr
}

// batch 发起一次 JSON-RPC 批量调用。注意 BatchCallContext 在 HTTP 层成功时返回 nil，
// 单个请求的错误在各自的 BatchElem.Error 里，需要调用方自行检查。
func (p *rpcPool) batch(ctx context.Context, elems []rpc.BatchElem) error {
	var lastErr error
	for attempt := 0; attempt < len(p.eps); attempt++ {
		ep := p.current()
		err := ep.client.BatchCallContext(ctx, elems)
		if err == nil {
			p.markOK(ep)
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return err
		}
		if !p.rotateOn(ep, err) {
			return err
		}
	}
	return lastErr
}

// current 当前活动端点
func (p *rpcPool) current() *endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.eps[p.active]
}

// chainID 池的链 ID
func (p *rpcPool) chainID() uint64 { return p.wantID }

// preferred 当前是否就在首选端点上。watcher 用它区分「链真的回滚了」和
// 「切到了一个落后的节点」——后者不该触发游标回退。
func (p *rpcPool) preferred() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active == 0
}

// markOK 端点调用成功，清空它的失败计数
func (p *rpcPool) markOK(ep *endpoint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ep.fails != 0 {
		ep.fails, ep.downUntil = 0, time.Time{}
	}
}

// rotateOn 判断这次失败要不要换端点，需要换则换掉并返回 true。
// 只有传输层与限流类错误值得换——参数错误、方法不支持换到哪都一样。
func (p *rpcPool) rotateOn(ep *endpoint, err error) bool {
	switch classify(err) {
	case classTransport, classThrottle:
	default:
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	ep.fails++
	ep.downUntil = time.Now().Add(backoffFor(ep.fails))
	p.logf("chainkit: RPC 端点 %s 连续失败 %d 次，冷却至 %s（%v）",
		redactURL(ep.url), ep.fails, ep.downUntil.Format("15:04:05"), err)

	now := time.Now()
	for i := 1; i <= len(p.eps); i++ {
		j := (p.active + i) % len(p.eps)
		if !p.eps[j].downUntil.After(now) {
			p.active = j
			p.logf("chainkit: RPC 切换至 %s", redactURL(p.eps[j].url))
			return true
		}
	}
	// 全部端点都在冷却：不换，把错误交出去让调用方退避
	return false
}

// probePreferred 若当前不在首选端点且首选已过冷却期，用廉价的 eth_blockNumber
// 探测一次，通了就切回去。探测失败会重置它的冷却。
func (p *rpcPool) probePreferred(ctx context.Context) {
	p.mu.Lock()
	if p.active == 0 {
		p.mu.Unlock()
		return
	}
	ep0 := p.eps[0]
	if ep0.downUntil.After(time.Now()) {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var n hexutil.Uint64
	if err := ep0.client.CallContext(probeCtx, &n, "eth_blockNumber"); err != nil {
		p.mu.Lock()
		ep0.fails++
		ep0.downUntil = time.Now().Add(backoffFor(ep0.fails))
		p.mu.Unlock()
		return
	}

	p.mu.Lock()
	ep0.fails, ep0.downUntil = 0, time.Time{}
	p.active = 0
	p.mu.Unlock()
	p.logf("chainkit: RPC 回归首选端点 %s", redactURL(ep0.url))
}

// close 关闭全部连接
func (p *rpcPool) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ep := range p.eps {
		if ep.client != nil {
			ep.client.Close()
			ep.client = nil
		}
	}
	return nil
}

// redactURL 抹掉 URL 的主体路径。公共节点的 RPC 地址常把密钥放在路径里
// （如 https://xxx/v2/<KEY>），原样打进日志等于公开密钥。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "***"
	}
	return u.Scheme + "://" + u.Host + "/***"
}
