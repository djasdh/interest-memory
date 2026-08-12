// Package usage tracks LLM/embedding token usage, aggregated per day and
// persisted through a caller-supplied flush callback.
package usage

import (
	"sync"
	"time"
)

// Usage is a single token-usage delta.
type Usage struct {
	Input    int64 // prompt/input tokens
	Output   int64 // completion/output tokens
	CacheHit int64 // input tokens served from the provider's prompt cache
}

// todayKey returns the UTC date key (YYYY-MM-DD) for the current instant.
func todayKey() string {
	return time.Now().UTC().Format("2006-01-02")
}

// Tracker accumulates usage for the current UTC day and persists each delta
// through the flush callback (day-scoped, so the callback upserts one row).
// It is safe for concurrent use.
type Tracker struct {
	mu      sync.Mutex
	today   string
	acc     Usage
	persist func(date string, u Usage) error
}

// NewTracker builds a Tracker. persist is called once per Add with the
// current date key and the exact delta (best-effort; errors are ignored).
func NewTracker(persist func(date string, u Usage) error) *Tracker {
	return &Tracker{today: todayKey(), persist: persist}
}

// Add accumulates a usage delta for the current day and persists it. On a
// day rollover the in-memory accumulator resets (the persisted rows keep the
// per-day history).
func (t *Tracker) Add(u Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	day := todayKey()
	if day != t.today {
		t.today = day
		t.acc = Usage{}
	}
	t.acc.Input += u.Input
	t.acc.Output += u.Output
	t.acc.CacheHit += u.CacheHit
	if t.persist != nil {
		_ = t.persist(day, u)
	}
}

// Today returns the current day key and its accumulated in-memory usage.
func (t *Tracker) Today() (string, Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if todayKey() != t.today {
		t.today = todayKey()
		t.acc = Usage{}
	}
	return t.today, t.acc
}
