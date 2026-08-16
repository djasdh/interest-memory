package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/djasdh/interest-memory/internal/store"
)

// Regression for the global-lock deadlock: Enqueue used to send on the
// per-agent queue while holding w.mu. When one agent's 64-slot queue filled
// (slow pipeline, burst of EndSession pushes), the 65th Enqueue blocked
// holding the global mutex, which froze GetJob, setStatus and every other
// agent's Enqueue — and because the draining worker calls setStatus (needs
// the same mutex) after each job, the whole worker deadlocked permanently.
//
// The fix sends on the queue outside the lock (backpressure no longer holds
// the global mutex). This test enqueues more jobs than the per-agent buffer
// with a slow processor: every Enqueue must complete and every job must
// reach StatusDone.
func TestEnqueueBackpressureDoesNotHoldGlobalLock(t *testing.T) {
	st := newWorkerTestStore(t)
	ctx := context.Background()
	const n = 70 // > per-agent queue capacity (64)
	for i := 0; i < n; i++ {
		sid := fmt.Sprintf("s%02d", i)
		if err := st.SaveTranscript(ctx, store.Transcript{SessionID: sid, AgentID: "agent-a", TurnCount: 1, RawTurns: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	fp := &fakeProcessor{} // 50ms per job: queue fills while jobs drain slowly
	w := New(fp, st, 5*time.Minute)
	defer w.Close()

	ids := make([]string, n)
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			id, err := w.Enqueue(ctx, "agent-a", fmt.Sprintf("s%02d", i))
			if err != nil {
				errCh <- err
				return
			}
			ids[i] = id
			errCh <- nil
		}(i)
	}
	// All enqueues must complete: backpressure is per-agent, it must never
	// block the global mutex. Under the old code the 65th Enqueue froze the
	// worker and this loop timed out.
	for i := 0; i < n; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("enqueue %d blocked: a full per-agent queue is holding a global resource", i)
		}
	}

	// Every job must reach StatusDone (the worker keeps draining).
	deadline := time.Now().Add(30 * time.Second)
	for {
		allDone := true
		for _, id := range ids {
			j, err := w.GetJob(ctx, id)
			if err != nil {
				t.Fatalf("GetJob(%s): %v", id, err)
			}
			if j.Status != StatusDone {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs stuck after backpressure: worker not draining")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
