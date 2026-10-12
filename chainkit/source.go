package chainkit

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
)

// defaultLogf 默认日志输出
func defaultLogf(format string, args ...any) { log.Printf(format, args...) }

// source 一种链的读写数据源。链无关的扫描循环（watcher.go）只依赖这个接口，
// 新增链类型时只需再实现一份它。
type source interface {
	// Head 当前链头高度
	Head(ctx context.Context) (uint64, error)
	// ChainID 链 ID
	ChainID(ctx context.Context) (uint64, error)
	// ScanRange 扫描 [from, to] 闭区间内所有命中关注地址的转账，按发现顺序逐笔 emit。
	// emit 返回 error 时立即中止并原样返回——调用方据此决定不推进游标。
	ScanRange(ctx context.Context, from, to uint64, emit func(*Transfer) error) error
	// Balance 查询余额（最小单位）
	Balance(ctx context.Context, addr string, tok *Token) (*big.Int, error)
	// Call 发一次只读 eth_call，返回原始返回数据。gas 为 0 时不带该字段。
	Call(ctx context.Context, to string, data []byte, gas uint64) ([]byte, error)
	// Send 签名并广播一笔转账，返回交易哈希
	Send(ctx context.Context, req *SendRequest) (string, error)
	// Probe 探测被切走的首选端点是否已恢复。链类型没有多端点概念时可为空实现。
	Probe(ctx context.Context)
	// Preferred 当前是否在首选端点上。用于区分「链回滚了」和「切到了落后的节点」。
	Preferred() bool
	// Close 释放连接
	Close() error
}

// newSource 按链类型创建数据源
func newSource(cfg *Config) (source, error) {
	switch cfg.Type {
	case "evm":
		return newEVMSource(cfg)
	case "chainos":
		return nil, errors.New("chainkit: chainos 数据源尚未实现（当前只支持 evm）")
	default:
		return nil, fmt.Errorf("chainkit: 未知链类型 %q（当前只支持 evm）", cfg.Type)
	}
}

// errClass 错误分类。分这个类是因为「换一个 RPC 端点」只对其中一部分错误有效：
// 拿节点限流去换端点是对的，拿参数错误去换端点是徒劳的。
type errClass int

const (
	classOther           errClass = iota // 业务/语义错误，换节点无用
	classTransport                       // 网络层、5xx：换节点有效
	classThrottle                        // 限流：换节点有效，但绝不能靠拆小请求来绕过
	classRangeLimit                      // 结果集过大（"query exceeds max results"）：靠缩小查询区间解决
	classRequestTooLarge                 // 请求体本身过大（HTTP 400/413）：靠缩小数组/批量解决
	classMethodMissing                   // 节点不支持该方法：要靠换调用路径解决
)

// classify 判定错误类别。顺序有意为之：限流必须在区间判断之前——
// 限流报错里也常带 "limit" 字样，误判成区间过大会导致请求被拆碎，反而把限流撞得更死。
func classify(err error) errClass {
	if err == nil {
		return classOther
	}

	// HTTP 状态码是最高可信的信号，不靠文本匹配。
	// 注意 go-ethereum 返回的是 HTTPError 值而不是指针（rpc/http.go），
	// 两种形态都要认，只认指针的话这段判断会静默失效、退化成文本匹配。
	if status, ok := httpStatusOf(err); ok {
		return classifyHTTPStatus(status)
	}

	msg := strings.ToLower(err.Error())
	if containsAny(msg, "too many requests", "rate limit", "rate-limit", "quota exceeded",
		"exceeded the quota", "slow down", "too frequent") {
		return classThrottle
	}
	// 必须排在区间判断之前：这些短语里也含 "too large"，会被下面的区间规则截胡，
	// 结果就是拿「地址数组太大」去二分区间，白拆一场
	if containsAny(msg, "request entity too large", "payload too large", "request too large",
		"request body too large", "content too large") {
		return classRequestTooLarge
	}
	if containsAny(msg,
		"query returned more than", "more than 10000 results", "block range", "blocks range",
		"range is too large", "range too large", "exceeds the range", "too large", "response size",
		"query timeout", "timeout exceeded",
	) {
		return classRangeLimit
	}
	if containsAny(msg, "does not exist/is not available", "method not found",
		"method does not exist", "unsupported method", "not supported") {
		return classMethodMissing
	}

	// JSON-RPC 错误对象：节点正常应答但方法/参数有问题
	if je, ok := asRPCError(err); ok {
		switch je.ErrorCode() {
		case -32601:
			return classMethodMissing
		case -32602:
			// 参数不合法里最常见的就是 topics 数组被拒
			return classRangeLimit
		}
		return classOther
	}

	// 剩下的是传输层：连接被拒、DNS、EOF、超时、上下文之外的网络中断
	return classTransport
}

// classifyHTTPStatus 按 HTTP 状态码归类
func classifyHTTPStatus(status int) errClass {
	switch {
	case status == http.StatusTooManyRequests:
		return classThrottle
	case status == http.StatusRequestEntityTooLarge, status == http.StatusRequestURITooLong:
		return classRequestTooLarge
	case status == http.StatusBadRequest:
		// 400 语义上是「请求不合法」，但实测不少节点把「批量/请求体太大」也报成 400
		// （12 个区块的收据批量直接回 400，减到 2 个就正常）。归到请求过大，
		// 让调用方去缩小请求；真是不合法的参数，缩小之后依然会失败并上抛。
		return classRequestTooLarge
	case status >= 500:
		return classTransport
	}
	return classOther
}

// isBatchTooLarge 批量请求被节点拒收（可能是批量太大，也可能是结果集太大）。
// 用于批量取块/批量取收据：那两处没有「结果集上限」的概念，两种原因都只能靠缩批解决。
func isBatchTooLarge(err error) bool {
	switch classify(err) {
	case classRequestTooLarge, classRangeLimit:
		return true
	}
	return false
}

// httpStatusOf 取错误里的 HTTP 状态码（go-ethereum 返回值类型，不是指针）
func httpStatusOf(err error) (int, bool) {
	var he rpc.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode, true
	}
	var hep *rpc.HTTPError
	if errors.As(err, &hep) && hep != nil {
		return hep.StatusCode, true
	}
	return 0, false
}

// asRPCError 判定是否为节点返回的 JSON-RPC 错误对象
func asRPCError(err error) (rpc.Error, bool) {
	var je rpc.Error
	if errors.As(err, &je) {
		return je, true
	}
	return nil, false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// IsThrottled 是否是节点限流。限流是瞬态的，正确反应是整批放弃、按指数退避等
// 限流窗口过去；把它当成「能力不支持」降级成更碎的请求，只会成倍放大请求量。
func IsThrottled(err error) bool { return classify(err) == classThrottle }

// IsRangeLimit 是否是查询区间/结果集过大
func IsRangeLimit(err error) bool { return classify(err) == classRangeLimit }

// IsMethodMissing 是否是节点不支持该方法
func IsMethodMissing(err error) bool { return classify(err) == classMethodMissing }

// backoffFor 连续失败 n 次后的等待时长：1s 起翻倍、封顶 60s，带 ±20% 抖动。
// 抖动是为了避免多条链在同一节点上锁步重试，反复撞进同一个限流窗口。
func backoffFor(n int) time.Duration {
	const (
		base = time.Second
		maxD = 60 * time.Second
	)
	d := base << min(max(n-1, 0), 6) // 1,2,4,8,16,32,64
	if d > maxD {
		d = maxD
	}
	return d*4/5 + time.Duration(rand.Int64N(int64(d*2/5)+1)) // [0.8d, 1.2d]
}

// sleepCtx 可被 ctx 打断的等待。返回 false 表示 ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
