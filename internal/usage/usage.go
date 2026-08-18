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

// Tracker accumulates usage for the current UTC day and persists aggregated
// deltas through the flush callback (day-scoped, so the callback upserts one
// row). With a positive flushInterval deltas are batched and flushed on a
// timer (reducing SQLite write amplification); interval <= 0 persists
// synchronously on every Add (backwards-compatible). It is safe for
// concurrent use.
type Tracker struct {
	mu       sync.Mutex
	today    string
	acc      Usage
	dirty    bool
	persist  func(date string, u Usage) error
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

// NewTracker builds a Tracker. persist is called with the current date key and
// the aggregated delta (best-effort; errors are ignored). flushInterval <= 0
// runs in synchronous mode (persist once per Add); otherwise a background
// goroutine flushes every flushInterval and Close flushes any remainder.
func NewTracker(persist func(date string, u Usage) error, flushInterval time.Duration) *Tracker {
	t := &Tracker{today: todayKey(), persist: persist, interval: flushInterval,
		stop: make(chan struct{}), done: make(chan struct{})}
	if flushInterval <= 0 {
		return t // sync mode: no goroutine
	}
	go t.flushLoop()
	return t
}

func (t *Tracker) flushLoop() {
	defer close(t.done)
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.flush()
		case <-t.stop:
			t.flush()
			return
		}
	}
}

func (t *Tracker) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.dirty {
		return
	}
	day := todayKey()
	if day != t.today {
		t.today = day
		t.acc = Usage{}
		t.dirty = false
		return
	}
	if t.persist != nil {
		_ = t.persist(day, t.acc)
	}
	t.acc = Usage{}
	t.dirty = false
}

// Add accumulates a usage delta for the current day. In sync mode (interval
// <= 0) it persists the delta immediately; otherwise the delta is batched
// until the next flush. On a day rollover the in-memory accumulator resets
// (the persisted rows keep the per-day history).
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
	t.dirty = true
	if t.interval <= 0 && t.persist != nil {
		_ = t.persist(day, u)
	}
}

// Today returns the current day key and its accumulated in-memory usage
// (including deltas not yet flushed).
func (t *Tracker) Today() (string, Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if todayKey() != t.today {
		t.today = todayKey()
		t.acc = Usage{}
	}
	return t.today, t.acc
}

// Close stops the background flusher and persists any pending delta. It is a
// no-op in sync mode (interval <= 0). Safe to call once.
func (t *Tracker) Close() {
	if t.interval <= 0 {
		return
	}
	close(t.stop)
	<-t.done
}
