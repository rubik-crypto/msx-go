package daylimit

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTakeAndUsed(t *testing.T) {
	s := New()
	if !s.Take("u1", "2026-11-11") {
		t.Fatal("第一次应该占得上")
	}
	if s.Take("u1", "2026-11-11") {
		t.Fatal("同一天第二次不该占得上")
	}
	if !s.Used("u1", "2026-11-11") {
		t.Fatal("Used 应该是 true")
	}
	if s.Used("u1", "2026-11-12") {
		t.Fatal("换一天就不算用过了")
	}
	if !s.Take("u1", "2026-11-12") {
		t.Fatal("第二天应该能再占一次")
	}
	if s.Used("u2", "2026-11-12") {
		t.Fatal("别的键不受影响")
	}
	if got := s.Last("u1"); got != "2026-11-12" {
		t.Fatalf("Last = %q", got)
	}
	// 空键 / 空日期不算数
	if s.Take("", "2026-11-11") || s.Take("u3", "") {
		t.Fatal("空键或空日期不该占得上")
	}
}

func TestReleaseAndPrune(t *testing.T) {
	s := New()
	s.Take("a", "2026-11-11")
	s.Take("b", "2026-11-12")
	s.Release("a")
	if s.Used("a", "2026-11-11") {
		t.Fatal("Release 之后应该能重新占")
	}
	keys := s.UsedKeys("2026-11-12")
	if len(keys) != 1 || keys[0] != "b" {
		t.Fatalf("UsedKeys = %v", keys)
	}
	if n := s.Prune("2026-11-12"); n != 0 {
		t.Fatalf("已经只剩当天了，Prune 删掉 %d 条", n)
	}
	s.Take("c", "2026-11-13")
	if n := s.Prune("2026-11-12"); n != 1 {
		t.Fatalf("Prune 应该删掉 1 条，实际 %d", n)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d", s.Len())
	}
}

// 并发抢同一天的名额：只能有一个成功。用 -race 跑更有意义。
func TestTakeConcurrent(t *testing.T) {
	s := New()
	var won int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Take("same", "2026-11-11") {
				atomic.AddInt64(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("应该只有一个成功，实际 %d", won)
	}
}

// 嵌进状态结构体的写法：**值字段**（零值就能用，落盘读回来也不用初始化）。
type holder struct {
	Name  string `json:"name"`
	Quota Set    `json:"quota"`
}

func TestJSONRoundTrip(t *testing.T) {
	h := holder{Name: "x"}
	if !h.Quota.Take("k", "2026-11-11") {
		t.Fatal("零值（没建过）就该能占名额")
	}
	b, err := json.Marshal(&h)
	if err != nil {
		t.Fatal(err)
	}
	var back holder
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Quota.Used("k", "2026-11-11") {
		t.Fatalf("读回来丢了记录：%s", b)
	}
	if back.Quota.Take("k", "2026-11-11") {
		t.Fatal("读回来之后当天不该能再占一次")
	}
	// 空集合写成 {}（不是 null），读回来零值也能直接用
	empty := holder{Name: "y"}
	b2, _ := json.Marshal(&empty)
	if string(b2) != `{"name":"y","quota":{}}` {
		t.Fatalf("空集合应该写成 {}，实际 %s", b2)
	}
	var back2 holder
	if err := json.Unmarshal(b2, &back2); err != nil {
		t.Fatal(err)
	}
	if !back2.Quota.Take("z", "2026-11-11") {
		t.Fatal("读回来之后应该是个能用的空表")
	}
	// 老数据里字段是 null：读回来照样能用（不能留一个 nil map 等着写的时候炸）
	var back3 holder
	if err := json.Unmarshal([]byte(`{"name":"z","quota":null}`), &back3); err != nil {
		t.Fatal(err)
	}
	if !back3.Quota.Take("k", "2026-11-11") {
		t.Fatal("null 读回来之后应该能用")
	}
	// 字段整个缺（更老的数据）：零值可用
	var back4 holder
	if err := json.Unmarshal([]byte(`{"name":"w"}`), &back4); err != nil {
		t.Fatal(err)
	}
	if !back4.Quota.Take("k", "2026-11-11") {
		t.Fatal("缺字段读回来之后应该能用")
	}
}
