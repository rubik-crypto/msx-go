package authsession

import (
	"sync"
	"time"
)

const (
	defaultMaxFailures = 8
	defaultWindow      = 10 * time.Minute
	defaultBlock       = 10 * time.Minute
	maxThrottleKeys    = 1024
)

type failureRecord struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

// Throttle 按来源记录登录失败次数：在 Window 内失败达到 MaxFailures 后，
// 这个来源会被锁住 Block 那么久。它只记录失败，不做密码校验。
//
// 一个 Throttle 可以被多个请求并发使用。来源的键由调用方决定，
// 通常用连接的对端地址；不要直接用可以伪造的请求头。
type Throttle struct {
	mu       sync.Mutex
	entries  map[string]*failureRecord
	max      int
	window   time.Duration
	blockFor time.Duration
}

// NewThrottle 创建登录失败限速器。参数不合法时使用默认值（8 次 / 10 分钟 / 锁 10 分钟）。
func NewThrottle(maxFailures int, window, block time.Duration) *Throttle {
	if maxFailures <= 0 {
		maxFailures = defaultMaxFailures
	}
	if window <= 0 {
		window = defaultWindow
	}
	if block <= 0 {
		block = defaultBlock
	}
	return &Throttle{
		entries:  make(map[string]*failureRecord),
		max:      maxFailures,
		window:   window,
		blockFor: block,
	}
}

// BlockedFor 返回这个来源还需要等多久才能再试；0 表示没有被锁。
func (t *Throttle) BlockedFor(key string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	item := t.entries[key]
	if item == nil {
		return 0
	}
	if item.blockedUntil.After(now) {
		return item.blockedUntil.Sub(now)
	}
	// 锁已经过去，或者这条失败记录已经超出统计窗口，都清掉重新算。
	if !item.blockedUntil.IsZero() || now.Sub(item.windowStart) > t.window {
		delete(t.entries, key)
	}
	return 0
}

// Fail 记一次登录失败。达到上限时开始锁定。
func (t *Throttle) Fail(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*failureRecord)
	}
	item := t.entries[key]
	if item == nil || now.Sub(item.windowStart) > t.window {
		item = &failureRecord{windowStart: now}
		t.entries[key] = item
	}
	item.count++
	if item.count >= t.max {
		item.blockedUntil = now.Add(t.blockFor)
		item.count = 0
		item.windowStart = now
	}
	if len(t.entries) > maxThrottleKeys {
		for k, v := range t.entries {
			if v.blockedUntil.IsZero() && now.Sub(v.windowStart) > t.window {
				delete(t.entries, k)
			}
		}
	}
}

// Reset 清掉一个来源的失败记录，通常在校验成功后调用。
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}
