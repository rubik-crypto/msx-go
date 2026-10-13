package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestAllowUpToLimit(t *testing.T) {
	now := time.Now()
	c := New(3, time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := c.AllowAt("a", now); !ok {
			t.Fatalf("第 %d 次应当放行", i+1)
		}
	}
	okRun, wait := c.AllowAt("a", now)
	if okRun {
		t.Fatal("第 4 次应当被拒")
	}
	if wait <= 0 {
		t.Fatalf("被拒时要给出正数等待秒数，拿到 %d", wait)
	}
	// 被拒的那次不该被记进窗口。
	if n := c.CountAt("a", now); n != 3 {
		t.Fatalf("被拒之后计数应当还是 3，拿到 %d", n)
	}
}

func TestWindowSlides(t *testing.T) {
	now := time.Now()
	c := New(1, 30*time.Millisecond)
	if ok, _ := c.AllowAt("a", now); !ok {
		t.Fatal("第一次应当放行")
	}
	if ok, _ := c.AllowAt("a", now.Add(10*time.Millisecond)); ok {
		t.Fatal("窗口内第二次应当被拒")
	}
	if ok, _ := c.AllowAt("a", now.Add(40*time.Millisecond)); !ok {
		t.Fatal("滑出窗口之后应当再放行")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	now := time.Now()
	c := New(1, time.Hour)
	if ok, _ := c.AllowAt("a", now); !ok {
		t.Fatal("a 应当放行")
	}
	if ok, _ := c.AllowAt("a", now); ok {
		t.Fatal("a 第二次应当被拒")
	}
	if ok, _ := c.AllowAt("b", now); !ok {
		t.Fatal("换一个键应当放行（限流是按来源算的）")
	}
}

func TestZeroLimitMeansUnlimited(t *testing.T) {
	now := time.Now()
	var nilCounter *Counter
	if ok, _ := nilCounter.AllowAt("a", now); !ok {
		t.Fatal("nil 计数器应当一律放行")
	}
	for _, c := range []*Counter{New(0, time.Hour), New(-1, time.Hour)} {
		for i := 0; i < 100; i++ {
			if ok, _ := c.AllowAt("a", now); !ok {
				t.Fatalf("limit <= 0 应当表示不限次，第 %d 次被拒了", i+1)
			}
		}
	}
}

func TestResetAndCount(t *testing.T) {
	now := time.Now()
	c := New(2, time.Hour)
	c.AllowAt("a", now)
	c.AllowAt("a", now.Add(time.Minute))
	if n := c.CountAt("a", now.Add(time.Minute)); n != 2 {
		t.Fatalf("窗口内计数应当是 2，拿到 %d", n)
	}
	c.Reset("a")
	if n := c.CountAt("a", now); n != 0 {
		t.Fatalf("Reset 之后应当是 0，拿到 %d", n)
	}
	if ok, _ := c.AllowAt("a", now); !ok {
		t.Fatal("Reset 之后应当重新放行")
	}
}

// 键只增不减会涨成内存泄漏：超过 maxKeys 时要把窗口已空的键清掉。
func TestSweepKeepsMemoryBounded(t *testing.T) {
	now := time.Now()
	c := New(5, 50*time.Millisecond)
	c.maxKeys = 8
	for i := 0; i < 40; i++ {
		c.AllowAt(string(rune('a'+i%26))+string(rune('0'+i%10)), now.Add(time.Duration(i%3)*time.Millisecond))
	}
	// 时间往后推远超窗口，再触发一次 sweep。
	c.AllowAt("zz", now.Add(time.Second))
	if len(c.hits) > c.maxKeys+1 {
		t.Fatalf("清理之后键数应当收住，拿到 %d", len(c.hits))
	}
}

func TestConcurrentUse(t *testing.T) {
	c := New(1000, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.Allow("same")
			}
		}()
	}
	wg.Wait()
	if n := c.Count("same"); n != 1000 {
		t.Fatalf("并发 20×50 次调用应当记满 1000 次，拿到 %d", n)
	}
	if ok, _ := c.Allow("same"); ok {
		t.Fatal("记满之后应当被拒")
	}
}
