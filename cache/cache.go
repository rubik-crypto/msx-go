// Package cache 一个带 TTL 的通用内存缓存，泛型键值，并发安全。
//
// 过期是「惰性」的：到点后 Get 会当它不存在，条目本身留到下次写入或 Delete 才清。
// 对单个小站够用；要主动清扫就自己定期 Delete。
package cache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

// Cache 通用内存缓存，支持 TTL 过期。
type Cache[K comparable, V any] struct {
	mu         sync.RWMutex
	items      map[K]entry[V]
	defaultTTL time.Duration
}

// New 创建缓存实例；defaultTTL 为 0 表示永不过期。
func New[K comparable, V any](defaultTTL time.Duration) *Cache[K, V] {
	return &Cache[K, V]{
		items:      make(map[K]entry[V]),
		defaultTTL: defaultTTL,
	}
}

// Get 取缓存值；不存在或已过期返回 false。
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || (!e.expiresAt.IsZero() && time.Now().After(e.expiresAt)) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Set 写入缓存，用默认 TTL。
func (c *Cache[K, V]) Set(key K, value V) {
	c.SetWithTTL(key, value, c.defaultTTL)
}

// SetWithTTL 写入缓存，用自定义 TTL（0 表示永不过期）。
func (c *Cache[K, V]) SetWithTTL(key K, value V, ttl time.Duration) {
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	c.mu.Lock()
	c.items[key] = entry[V]{value: value, expiresAt: expiresAt}
	c.mu.Unlock()
}

// Delete 删除一个键。
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}
