package authsession

import (
	"sync"
	"testing"
	"time"
)

func TestThrottleBlocksAfterFailures(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	th := NewThrottle(3, time.Minute, 5*time.Minute)
	const key = "203.0.113.1"

	if wait := th.BlockedFor(key, now); wait != 0 {
		t.Fatalf("开始不应该被锁: %v", wait)
	}
	th.Fail(key, now)
	th.Fail(key, now)
	if wait := th.BlockedFor(key, now); wait != 0 {
		t.Fatalf("失败次数没到上限不应该锁: %v", wait)
	}
	th.Fail(key, now)
	if wait := th.BlockedFor(key, now); wait != 5*time.Minute {
		t.Fatalf("达到上限后应该锁 5 分钟，实际 %v", wait)
	}
	if wait := th.BlockedFor(key, now.Add(4*time.Minute)); wait != time.Minute {
		t.Fatalf("锁定期内剩余时间不对: %v", wait)
	}
	if wait := th.BlockedFor(key, now.Add(5*time.Minute)); wait != 0 {
		t.Fatalf("锁到期后应该放行: %v", wait)
	}
}

func TestThrottleWindowAndReset(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	th := NewThrottle(2, time.Minute, time.Hour)
	const key = "203.0.113.2"

	th.Fail(key, now)
	// 超过统计窗口后重新计数，之前的失败不再累加。
	th.Fail(key, now.Add(2*time.Minute))
	if wait := th.BlockedFor(key, now.Add(2*time.Minute)); wait != 0 {
		t.Fatalf("跨窗口后不应该被锁: %v", wait)
	}
	th.Fail(key, now.Add(2*time.Minute))
	if wait := th.BlockedFor(key, now.Add(2*time.Minute)); wait == 0 {
		t.Fatal("同一窗口内第二次失败后应该被锁")
	}

	th.Reset(key)
	if wait := th.BlockedFor(key, now.Add(2*time.Minute)); wait != 0 {
		t.Fatal("Reset 后应该放行")
	}
}

func TestThrottleConcurrent(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	th := NewThrottle(1000, time.Minute, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				th.Fail("same", now)
				th.BlockedFor("same", now)
			}
		}()
	}
	wg.Wait()
}

func TestThrottleDefaults(t *testing.T) {
	th := NewThrottle(0, 0, 0)
	if th.max != defaultMaxFailures || th.window != defaultWindow || th.blockFor != defaultBlock {
		t.Fatalf("默认参数不对: %+v", th)
	}
}
