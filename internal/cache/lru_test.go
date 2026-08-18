package cache

import (
	"sync"
	"testing"
)

func TestLRUBasic(t *testing.T) {
	c := New[string, int](2)
	if _, ok := c.Get("a"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Set("a", 1)
	c.Set("b", 2)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %v,%v want 1,true", v, ok)
	}
	c.Set("c", 3) // evicts oldest = b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted (LRU)")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestLRUAccessUpdatesRecency(t *testing.T) {
	c := New[string, int](2)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a")    // touch a → a becomes most recent
	c.Set("c", 3) // evicts b (least recent now)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %v,%v want 1,true", v, ok)
	}
}

func TestLRUZeroCapacityDisabled(t *testing.T) {
	c := New[string, int](0)
	c.Set("a", 1)
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 for disabled cache", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("disabled cache should miss")
	}
}

// TestLRUConcurrentAccess guards the cache against data races: the embedding
// LRU is read/written from concurrent wiki-compile goroutines.
func TestLRUConcurrentAccess(t *testing.T) {
	c := New[int, int](64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				c.Set(base+i, i)
				_, _ = c.Get(base + i)
			}
		}(g * 1000)
	}
	wg.Wait()
	if c.Len() == 0 {
		t.Fatal("expected non-empty cache after concurrent writes")
	}
}
