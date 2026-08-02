package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"interest-memory/internal/store"
)

// fakeProcessor records processed sessions and can inject an error.
type fakeProcessor struct {
	mu      sync.Mutex
	seen    []string // "agent/session"
	fail    error
	running int
	overlap bool
}

func (f *fakeProcessor) ProcessSession(_ context.Context, agentID string, t store.Transcript) error {
	f.mu.Lock()
	f.running++
	if f.running > 1 {
		f.overlap = true
	}
	f.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	f.mu.Lock()
	f.running--
	f.seen = append(f.seen, agentID+"/"+t.SessionID)
	err := f.fail
	f.mu.Unlock()
	return err
}

func newWorkerTestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEnqueueAndDone(t *testing.T) {
	st := newWorkerTestStore(t)
	ctx := context.Background()
	if err := st.SaveTranscript(ctx, store.Transcript{SessionID: "s1", AgentID: "agent-a", TurnCount: 2, RawTurns: "[]"}); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProcessor{}
	w := New(fp, st)
	defer w.Close()

	jobID, err := w.Enqueue(ctx, "agent-a", "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Poll until done.
	deadline := time.Now().Add(3 * time.Second)
	for {
		j, err := w.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if j.Status == StatusDone {
			break
		}
		if j.Status == StatusFailed {
			t.Fatalf("job failed: %s", j.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job stuck in %s", j.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	fp.mu.Lock()
	seen := append([]string(nil), fp.seen...)
	fp.mu.Unlock()
	if len(seen) != 1 || seen[0] != "agent-a/s1" {
		t.Fatalf("processed = %v, want agent-a/s1", seen)
	}
}

func TestSerialPerAgent(t *testing.T) {
	st := newWorkerTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		sid := "s" + string(rune('a'+i))
		if err := st.SaveTranscript(ctx, store.Transcript{SessionID: sid, AgentID: "agent-a", TurnCount: 1, RawTurns: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	fp := &fakeProcessor{}
	w := New(fp, st)
	defer w.Close()

	for _, sid := range []string{"sa", "sb", "sc"} {
		if _, err := w.Enqueue(ctx, "agent-a", sid); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		fp.mu.Lock()
		done := len(fp.seen)
		fp.mu.Unlock()
		if done == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not all jobs processed in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fp.mu.Lock()
	overlap := fp.overlap
	fp.mu.Unlock()
	if overlap {
		t.Error("per-agent jobs overlapped (serialization broken)")
	}
}

func TestJobFailedKeepsTranscriptUnprocessed(t *testing.T) {
	st := newWorkerTestStore(t)
	ctx := context.Background()
	if err := st.SaveTranscript(ctx, store.Transcript{SessionID: "s1", AgentID: "agent-a", TurnCount: 1, RawTurns: "[]"}); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProcessor{fail: errors.New("boom")}
	w := New(fp, st)
	defer w.Close()

	jobID, _ := w.Enqueue(ctx, "agent-a", "s1")
	deadline := time.Now().Add(3 * time.Second)
	for {
		j, _ := w.GetJob(ctx, jobID)
		if j.Status == StatusFailed {
			if j.Error == "" {
				t.Error("failed job missing error")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job stuck in %s", j.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Transcript remains unprocessed for retry.
	list, err := st.ListUnprocessedTranscripts(ctx, "agent-a")
	if err != nil || len(list) != 1 {
		t.Fatalf("unprocessed after failure = %+v err=%v", list, err)
	}
}

func TestGetJobNotFound(t *testing.T) {
	st := newWorkerTestStore(t)
	w := New(&fakeProcessor{}, st)
	defer w.Close()
	if _, err := w.GetJob(context.Background(), "nope"); err != ErrNoJob {
		t.Fatalf("GetJob = %v, want ErrNoJob", err)
	}
}
