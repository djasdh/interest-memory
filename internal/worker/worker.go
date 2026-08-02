package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"interest-memory/internal/store"
)

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
// same agent cannot interleave (design §五: worker 串行).
type Worker struct {
	mu      sync.Mutex
	queues  map[string]chan jobItem
	jobs    map[string]*Job
	closed  bool
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
	w.mu.Unlock()

	w.enqueueItem(jobItem{jobID: jobID, agentID: agentID, session: sessionID})
	return jobID, nil
}

func (w *Worker) enqueueItem(it jobItem) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	ch, ok := w.queues[it.agentID]
	if !ok {
		ch = make(chan jobItem, 64)
		w.queues[it.agentID] = ch
		go w.runAgent(it.agentID, ch)
	}
	ch <- it
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
	now := time.Now()
	j.DoneAt = &now
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
// are no-ops.
func (w *Worker) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	for _, ch := range w.queues {
		close(ch)
	}
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
