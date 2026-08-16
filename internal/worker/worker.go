package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/djasdh/interest-memory/internal/store"
)

// maxJobs caps the in-memory job table. Enqueue evicts the oldest terminal
// (done/failed) jobs past this bound so a long-running process doesn't leak
// memory. Recent/terminal jobs stay queryable until capacity pressure evicts
// them.
const maxJobs = 1000

// Status of a job.
type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Job is one queued transcript-processing task (memory-resident).
type Job struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agent_id"`
	SessionID string     `json:"session_id"`
	Status    Status     `json:"status"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
}

// Processor processes a transcript for an agent (implemented by
// service.Service.ProcessSession).
type Processor interface {
	ProcessSession(ctx context.Context, agentID string, t store.Transcript) error
}

// ErrNoJob is returned by GetJob for unknown ids.
var ErrNoJob = errors.New("worker: job not found")

// Worker serializes transcript processing per agent: each agent gets a FIFO
// channel and a dedicated goroutine, so concurrent EndSession pushes for the
// same agent cannot interleave (worker serial).
type Worker struct {
	mu      sync.Mutex
	queues  map[string]chan jobItem
	jobs    map[string]*Job
	closed  bool
	done    chan struct{} // closed by Close; wakes senders blocked on a full queue
	svc     Processor
	store   store.Store
	timeout time.Duration
}

type jobItem struct {
	jobID   string
	agentID string
	session string
}

// New builds a worker. svc runs the pipeline; st supplies transcripts and
// the processed marker. Job state is kept in memory. jobTimeout caps each
// job's total runtime (the wiki stage is serial per-point agent loops, so a
// short timeout kills long sessions mid-pipeline — see config.WorkerConfig).
func New(svc Processor, st store.Store, jobTimeout time.Duration) *Worker {
	if jobTimeout <= 0 {
		jobTimeout = 45 * time.Minute
	}
	return &Worker{
		queues:  make(map[string]chan jobItem),
		jobs:    make(map[string]*Job),
		done:    make(chan struct{}),
		svc:     svc,
		store:   st,
		timeout: jobTimeout,
	}
}

// Enqueue queues a transcript for processing and returns a job id. The
// transcript must already be persisted by the caller.
func (w *Worker) Enqueue(ctx context.Context, agentID, sessionID string) (string, error) {
	jobID := newJobID()
	j := &Job{
		ID:        jobID,
		AgentID:   agentID,
		SessionID: sessionID,
		Status:    StatusQueued,
		CreatedAt: time.Now(),
	}
	w.mu.Lock()
	w.jobs[jobID] = j
	if len(w.jobs) > maxJobs {
		w.pruneLocked()
	}
	w.mu.Unlock()

	w.enqueueItem(jobItem{jobID: jobID, agentID: agentID, session: sessionID})
	return jobID, nil
}

// pruneLocked evicts the oldest terminal jobs until the table is back within
// maxJobs. Caller must hold w.mu. Running jobs are never evicted.
func (w *Worker) pruneLocked() {
	type term struct {
		id string
		at time.Time
	}
	var terms []term
	for id, j := range w.jobs {
		if (j.Status == StatusDone || j.Status == StatusFailed) && j.DoneAt != nil {
			terms = append(terms, term{id: id, at: *j.DoneAt})
		}
	}
	sort.Slice(terms, func(i, k int) bool { return terms[i].at.Before(terms[k].at) })
	excess := len(w.jobs) - maxJobs
	for i := 0; i < excess && i < len(terms); i++ {
		delete(w.jobs, terms[i].id)
	}
}

func (w *Worker) enqueueItem(it jobItem) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	ch, ok := w.queues[it.agentID]
	if !ok {
		ch = make(chan jobItem, 64)
		w.queues[it.agentID] = ch
		go w.runAgent(it.agentID, ch)
	}
	w.mu.Unlock()
	// Send OUTSIDE the lock: when an agent's queue is full, the send blocks
	// on backpressure. Holding the global mutex there would freeze GetJob,
	// setStatus and every other agent's Enqueue — and since the worker that
	// drains the queue calls setStatus (which needs the same mutex) after
	// finishing a job, the system deadlocks permanently.
	select {
	case ch <- it:
	case <-w.done: // closed while waiting: drop the item
	}
}

func (w *Worker) runAgent(agentID string, ch <-chan jobItem) {
	for it := range ch {
		w.process(it)
	}
}

func (w *Worker) process(it jobItem) {
	// Job lifecycle is independent of the HTTP request that enqueued it:
	// always run on a fresh context so a completed request can't cancel the
	// pipeline mid-flight.
	runCtx, cancel := context.WithTimeout(context.Background(), w.timeout)
	defer cancel()

	w.setStatus(it.jobID, StatusRunning, "")
	tx, err := w.store.GetTranscript(runCtx, it.agentID, it.session)
	if err != nil {
		w.setStatus(it.jobID, StatusFailed, err.Error())
		return
	}
	if tx == nil {
		w.setStatus(it.jobID, StatusFailed, "transcript not found")
		return
	}

	if err := w.svc.ProcessSession(runCtx, it.agentID, *tx); err != nil {
		// Keep transcript unprocessed so a retry can pick it up.
		w.setStatus(it.jobID, StatusFailed, err.Error())
		return
	}
	if err := w.store.MarkTranscriptProcessed(runCtx, it.agentID, it.session); err != nil {
		w.setStatus(it.jobID, StatusFailed, "mark processed: "+err.Error())
		return
	}
	w.setStatus(it.jobID, StatusDone, "")
}

func (w *Worker) setStatus(jobID string, status Status, errMsg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j, ok := w.jobs[jobID]
	if !ok {
		return
	}
	j.Status = status
	j.Error = errMsg
	if status == StatusDone || status == StatusFailed {
		// Only terminal states carry a completion timestamp; a running job
		// must keep DoneAt nil so clients polling done_at don't mistake a
		// live job for a finished one (and pruneLocked's ordering stays
		// terminal-only).
		now := time.Now()
		j.DoneAt = &now
	} else {
		j.DoneAt = nil
	}
}

// GetJob returns a job by id.
func (w *Worker) GetJob(_ context.Context, jobID string) (*Job, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j, ok := w.jobs[jobID]
	if !ok {
		return nil, ErrNoJob
	}
	copy := *j
	return &copy, nil
}

// Close stops all agent goroutines. Safe to call once; further Enqueue calls
// are no-ops. The done channel is closed first so senders blocked on a full
// queue (backpressure outside the lock) unblock instead of panicking on a
// closed queue channel.
func (w *Worker) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.done)
	for _, ch := range w.queues {
		close(ch)
	}
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
