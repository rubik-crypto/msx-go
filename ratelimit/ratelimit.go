// Package ratelimit 按来源键（IP、设备号、账号，任意字符串都行）做滑动窗口限流，
// 用来挡住「不需要密码、做完还倒送权益」的动作被脚本批量刷
// （注册送余额、领新人礼包、免费抽一次奖这类）。
//
// 它和「密码错几次就锁账号」那种失败限流器不是一回事，两者要分开：
// 失败限流器只数**失败**次数、锁的是「正在猜密码的人」；
// 这里的 Counter 把**成功也当成一次消费**——因为在这个场景里，
// 成功的那一次才是有害的那一次。用失败限流器去数成功，语义是拧的。
//
//	// 同一个来源一小时最多做 10 次
//	lim := ratelimit.New(10, time.Hour)
//	if okRun, wait := lim.Allow(clientIP(r)); !okRun {
//		// 回一句「请 N 秒后再试」，并**在写入之前**返回
//	}
//
// 计数器只活在内存里：进程重启就清空。这对防刷场景是够的（攻击者刷不动的是速率），
// 需要跨重启保留的配额请用别的存储。
package ratelimit

import (
	"sync"
	"time"
)

// DefaultMaxKeys 是内存里最多保留多少个来源键。
// 键是来源地址这类天然有长尾的东西，只增不减会慢慢涨成内存泄漏，
// 所以键数超过这个值时顺手清一遍窗口内已经没有记录的键。
const DefaultMaxKeys = 4096

// Counter 是一个按键计数的滑动窗口限流器，可以并发使用。
//
// 窗口是「滚动的」而不是「按自然时段切开的」：单个键的时间戳全部留着，
// 每次判断前把滑出窗口的那些剔掉。这样不会出现
// 「整点前打满一次、过一秒又打满一次」这种边界双倍放行。
type Counter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	hits    map[string][]time.Time
}

// New 造一个限流器：同一个键在 window 时间里最多放行 limit 次。
//
// **limit <= 0 表示不限次**（一律放行），这样调用方不必为「关掉限流」另写分支；
// window <= 0 当作 0 处理（每次调用都会把之前的记录剔掉，等价于不限次）。
func New(limit int, window time.Duration) *Counter {
	if window < 0 {
		window = 0
	}
	return &Counter{
		limit:   limit,
		window:  window,
		maxKeys: DefaultMaxKeys,
		hits:    make(map[string][]time.Time),
	}
}

// Allow 给这个键记一次，返回放不放行；不放行时第二个返回值是「还要等多少秒」
// （至少 1，方便直接拼进给人看的提示里）。
//
// **计数点应该放在所有校验都通过之后、真正写入之前**：参数填错一格就占掉
// 一次名额，正常用户会莫名其妙地撞上限制。
func (c *Counter) Allow(key string) (bool, int) {
	return c.AllowAt(key, time.Now())
}

// AllowAt 和 Allow 一样，只是用调用方给的时间当「现在」。
// 需要确定性的测试（或者把时间统一成服务端的钟）时用它。
func (c *Counter) AllowAt(key string, now time.Time) (bool, int) {
	if c == nil || c.limit <= 0 {
		return true, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.hits) > c.maxKeys {
		c.sweepLocked(now)
	}
	kept := make([]time.Time, 0, len(c.hits[key])+1)
	for _, t := range c.hits[key] {
		if now.Sub(t) < c.window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= c.limit {
		// 被拒的这一次**不记**：记下来的话，一直被拒的人会把自己的窗口
		// 一直往后推，越试越久。
		c.hits[key] = kept
		return false, int((c.window - now.Sub(kept[0])).Seconds()) + 1
	}
	c.hits[key] = append(kept, now)
	return true, 0
}

// Count 返回这个键在当前窗口里已经记了几次（不改变任何状态）。
func (c *Counter) Count(key string) int {
	return c.CountAt(key, time.Now())
}

// CountAt 和 Count 一样，只是用调用方给的时间当「现在」。
func (c *Counter) CountAt(key string, now time.Time) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.hits[key] {
		if now.Sub(t) < c.window {
			n++
		}
	}
	return n
}

// Reset 清掉一个键的计数（比如管理员核实之后手工放行某个人）。
func (c *Counter) Reset(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.hits, key)
}

// sweepLocked 丢掉窗口内已经没有记录的键（调用方必须持锁）。
func (c *Counter) sweepLocked(now time.Time) {
	for k, v := range c.hits {
		if len(v) == 0 || now.Sub(v[len(v)-1]) >= c.window {
			delete(c.hits, k)
		}
	}
}
