package usage

import (
	"sync"
	"testing"
	"time"
)

func TestTrackerAddAndPersist(t *testing.T) {
	var persisted map[string]Usage
	persist := func(date string, u Usage) error {
		if persisted == nil {
			persisted = map[string]Usage{}
		}
		persisted[date] = Usage{
			Input:    persisted[date].Input + u.Input,
			Output:   persisted[date].Output + u.Output,
			CacheHit: persisted[date].CacheHit + u.CacheHit,
		}
		return nil
	}
	tr := NewTracker(persist, 0)
	defer tr.Close()
	tr.Add(Usage{Input: 10, Output: 5, CacheHit: 3})
	tr.Add(Usage{Input: 2, Output: 1})

	day, acc := tr.Today()
	if day == "" {
		t.Fatalf("empty day key")
	}
	if acc.Input != 12 || acc.Output != 6 || acc.CacheHit != 3 {
		t.Errorf("acc = %+v, want input=12 output=6 cacheHit=3", acc)
	}
	got := persisted[day]
	if got.Input != 12 || got.Output != 6 || got.CacheHit != 3 {
		t.Errorf("persisted = %+v, want input=12 output=6 cacheHit=3", got)
	}
}

func TestTrackerNoPersistDoesNotPanic(t *testing.T) {
	tr := NewTracker(nil, 0)
	tr.Add(Usage{Input: 1})
	if _, acc := tr.Today(); acc.Input != 1 {
		t.Errorf("acc.Input = %d, want 1", acc.Input)
	}
}

func TestTrackerFlushesAfterInterval(t *testing.T) {
	var mu sync.Mutex
	var persisted map[string]Usage
	persist := func(date string, u Usage) error {
		mu.Lock()
		defer mu.Unlock()
		if persisted == nil {
			persisted = map[string]Usage{}
		}
		prev := persisted[date]
		persisted[date] = Usage{Input: prev.Input + u.Input, Output: prev.Output + u.Output, CacheHit: prev.CacheHit + u.CacheHit}
		return nil
	}
	tr := NewTracker(persist, 20*time.Millisecond)
	defer tr.Close()
	tr.Add(Usage{Input: 10})
	tr.Add(Usage{Input: 5})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	got := persisted[todayKey()]
	mu.Unlock()
	if got.Input != 15 {
		t.Errorf("persisted.Input = %d, want 15 (batched flush)", got.Input)
	}
}

func TestTrackerSyncPersistWhenIntervalZero(t *testing.T) {
	var persisted []Usage
	persist := func(_ string, u Usage) error { persisted = append(persisted, u); return nil }
	tr := NewTracker(persist, 0)
	tr.Add(Usage{Input: 1})
	if len(persisted) != 1 {
		t.Errorf("persist calls = %d, want 1 (sync when interval <= 0)", len(persisted))
	}
}

func TestTrackerCloseFlushesPending(t *testing.T) {
	var mu sync.Mutex
	var persisted map[string]Usage
	persist := func(date string, u Usage) error {
		mu.Lock()
		defer mu.Unlock()
		if persisted == nil {
			persisted = map[string]Usage{}
		}
		prev := persisted[date]
		persisted[date] = Usage{Input: prev.Input + u.Input, Output: prev.Output + u.Output, CacheHit: prev.CacheHit + u.CacheHit}
		return nil
	}
	tr := NewTracker(persist, 0)
	tr.Add(Usage{Input: 7})
	tr.Close()
	mu.Lock()
	got := persisted[todayKey()]
	mu.Unlock()
	if got.Input != 7 {
		t.Errorf("persisted.Input = %d, want 7 (Close flushes)", got.Input)
	}
}
