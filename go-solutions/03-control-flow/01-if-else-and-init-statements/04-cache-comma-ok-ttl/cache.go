package ttlcache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

type Cache[K comparable, V any] struct {
	mu    sync.RWMutex
	now   func() time.Time
	items map[K]entry[V]
}

func New[K comparable, V any](now func() time.Time) *Cache[K, V] {
	return &Cache[K, V]{
		now:   now,
		items: make(map[K]entry[V]),
	}
}

func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{
		value:     value,
		expiresAt: c.now().Add(ttl),
	}
}

func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; !ok || c.now().After(e.expiresAt) {
		if ok {
			delete(c.items, key)
		}
		var zero V
		return zero, false
	}
	return c.items[key].value, true
}

func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
