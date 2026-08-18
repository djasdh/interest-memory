// Package cache provides a small in-memory LRU used to dedupe expensive
// deterministic calls (embedding, recall). Capacity 0 disables caching.
// All methods are safe for concurrent use.
package cache

import (
	"container/list"
	"sync"
)

type Cache[K comparable, V any] struct {
	mu  sync.Mutex
	cap int
	ll  *list.List
	idx map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key K
	val V
}

func New[K comparable, V any](capacity int) *Cache[K, V] {
	return &Cache[K, V]{cap: capacity, ll: list.New(), idx: make(map[K]*list.Element)}
}

func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.idx)
}

func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	if c.cap <= 0 {
		return zero, false
	}
	if el, ok := c.idx[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*entry[K, V]).val, true
	}
	return zero, false
}

func (c *Cache[K, V]) Set(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cap <= 0 {
		return
	}
	if el, ok := c.idx[k]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*entry[K, V]).val = v
		return
	}
	el := c.ll.PushFront(&entry[K, V]{key: k, val: v})
	c.idx[k] = el
	if len(c.idx) > c.cap {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.idx, oldest.Value.(*entry[K, V]).key)
		}
	}
}
