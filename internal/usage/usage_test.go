package usage

import (
	"testing"
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
	tr := NewTracker(persist)
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
	tr := NewTracker(nil)
	tr.Add(Usage{Input: 1})
	if _, acc := tr.Today(); acc.Input != 1 {
		t.Errorf("acc.Input = %d, want 1", acc.Input)
	}
}
