// Package daylimit 记录「某个键在同一个自然日里用掉了没有」，并发安全，
// 可以直接嵌进需要 JSON 落盘的结构体里。
//
// 典型用途：每个账号每天只能领一次奖励、每台设备每天只能提交一次、
// 每个用户每天只能抽一次奖。跨天不用定时任务去清——值不等于今天就算没用过，
// 老日期留在表里不影响任何判断（调用方想清就调 Release 或 Prune）。
//
// 要点：**「查一下今天用过没有」和「占上今天的名额」必须是同一步**（Take）。
// 分开写成 Used + Set 两步的话，两个并发请求会各自看到「今天还没用过」，
// 于是同一份奖励发两次——这是这类功能最常见的一个漏洞。
//
//	set := daylimit.New()
//	if set.Take(userID, "2026-11-11") {
//		grant() // 只有第一个请求能走到这里
//	}
//
// Set 里带互斥锁，**不要复制**，一律用指针（*Set）传递和存放。
//
// 嵌进要落盘的状态结构体时用**值字段**最省事：
//
//	type Account struct {
//	    ID    string      `json:"id"`
//	    Quota daylimit.Set `json:"quota"` // 零值就能用，老数据里没有这个字段也没关系
//	}
//
// 值字段的零值（没建过、Map 为 nil）可以直接 Take，读到 JSON 里的 null 也能用。
// 落盘时把结构体的**指针**交给 json（json.Marshal(&state)）：Set 里带锁，
// 按值传会被复制一份，go vet 的 copylocks 会直接报出来。
//
// 反过来用
// 反过来写成 *Set 指针字段时，JSON 里的 null 会把指针置成 nil（encoding/json 的
// 规矩，不走 UnmarshalJSON），那种写法要在读盘之后自己补一个 Set.New()。
package daylimit

import (
	"encoding/json"
	"sort"
	"sync"
)

// Set 是「键 -> 最近一次占用的日期」。零值不可用，用 New 建。
type Set struct {
	mu sync.Mutex
	m  map[string]string
}

// New 建一个空的 Set。
func New() *Set { return &Set{m: map[string]string{}} }

// Take 试着占下 key 今天的名额：当天还没被占过就占上并返回 true，
// 已经被占过就什么都不动、返回 false。判断和占用在同一把锁里完成。
// day 由调用方给（"2026-11-11" 这种），这样跨时区、跨夏令时怎么算「一天」
// 由调用方自己定。
func (s *Set) Take(key, day string) bool {
	if key == "" || day == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	if s.m[key] == day {
		return false
	}
	s.m[key] = day
	return true
}

// Used 问 key 在 day 这一天有没有被占过。
func (s *Set) Used(key, day string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key] == day
}

// Last 返回 key 最近一次占用的日期（没有过就是空串）。
func (s *Set) Last(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key]
}

// Release 把 key 的名额还回去（比如占了名额之后的动作失败了）。
func (s *Set) Release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// UsedKeys 列出 day 这一天被占过的所有键，按字典序排好（方便测试和展示）。
func (s *Set) UsedKeys(day string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for k, d := range s.m {
		if d == day {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Len 是表里有多少条记录（含以前日期的）。
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Prune 丢掉不是 keep 这一天的记录，返回丢掉了几条。
// 想给落盘文件瘦身时调它；不调也不会影响判断。
func (s *Set) Prune(keep string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, d := range s.m {
		if d != keep {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// MarshalJSON 让 Set 能直接跟着宿主结构体一起落盘。
// 空集合写成 {}（不是 null），读回来还是个能用的 Set。
func (s *Set) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(s.m)
}

// UnmarshalJSON 读回落盘的记录。**这是它能嵌进已有状态结构体的关键**：
// 老数据里没有这个字段（读到 null / 空）时，Set 会自己变成一个能用的空表，
// 而不是留一个 nil map 等着写的时候炸掉。
func (s *Set) UnmarshalJSON(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(b) == 0 || string(b) == "null" {
		s.m = map[string]string{}
		return nil
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	s.m = m
	return nil
}
