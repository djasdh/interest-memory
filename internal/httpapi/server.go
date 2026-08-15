package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/djasdh/interest-memory/internal/recall"
	"github.com/djasdh/interest-memory/internal/service"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/worker"
)

// Service is the orchestration surface the HTTP layer calls (implemented by
// *service.Service).
type Service interface {
	ProcessSession(ctx context.Context, agentID string, t store.Transcript) error
	SaveTranscript(ctx context.Context, t store.Transcript) error
	Recall(ctx context.Context, agentID, query string, opts recall.Options) (string, error)
	Search(ctx context.Context, agentID, query string, topK int) ([]recall.Result, error)
	GetByID(ctx context.Context, agentID, id string) (*recall.Result, error)
	ListLogs(ctx context.Context, agentID string, limit, offset int) ([]store.ChangeLog, error)
	ListInterestPoints(ctx context.Context, agentID string) ([]store.InterestPoint, error)
	ListPages(ctx context.Context, agentID string, pageType store.PageType) ([]store.Page, error)
	ListPendingLinks(ctx context.Context, agentID string) ([]store.PendingLink, error)
	Stats(ctx context.Context, agentID string) (map[string]int, error)
	Usage(ctx context.Context, since string) ([]store.UsageRow, error)
	ForkManual(ctx context.Context, agentID string) (*store.Transcript, error)
	KanbanBoardExcluded(boardID, boardName string) bool
	ListGraph(ctx context.Context, agentID string) (*service.Graph, error)
}

// Worker is the async queue surface (implemented by *worker.Worker).
type Worker interface {
	Enqueue(ctx context.Context, agentID, sessionID string) (string, error)
	GetJob(ctx context.Context, jobID string) (*worker.Job, error)
}

// Server is the REST API server.
type Server struct {
	svc Service
	wk  Worker
}

// NewServer builds the REST server.
func NewServer(svc Service, wk Worker) *Server {
	return &Server{svc: svc, wk: wk}
}

// Route is one registered HTTP endpoint (Go 1.22 pattern with method prefix).
type Route struct {
	Pattern string
	Handler http.HandlerFunc
}

// Routes returns the memory endpoints for registration into an external
// mux (stage 5: gateway.Server). Health is provided by the gateway itself.
func (s *Server) Routes() []Route {
	return []Route{
		{Pattern: "POST /api/v1/{agent}/sessions", Handler: s.handleSessions},
		{Pattern: "GET /api/v1/{agent}/recall", Handler: s.handleRecall},
		{Pattern: "GET /api/v1/{agent}/interest-points", Handler: s.handleInterestPoints},
		{Pattern: "GET /api/v1/{agent}/wiki/pages", Handler: s.handleWikiPages},
		{Pattern: "GET /api/v1/{agent}/pending-links", Handler: s.handlePendingLinks},
		{Pattern: "GET /api/v1/{agent}/graph", Handler: s.handleGraph},
		{Pattern: "GET /api/v1/{agent}/graph.html", Handler: s.handleGraphHTML},
		{Pattern: "GET /api/v1/{agent}/search", Handler: s.handleSearch},
		{Pattern: "GET /api/v1/{agent}/logs", Handler: s.handleLogs},
		{Pattern: "POST /api/v1/{agent}/fork", Handler: s.handleFork},
		{Pattern: "GET /api/v1/{agent}/jobs/{id}", Handler: s.handleJob},
		{Pattern: "GET /api/v1/{agent}/stats", Handler: s.handleStats},
		{Pattern: "GET /api/v1/{agent}/usage", Handler: s.handleUsage},
	}
}

// Handler returns a self-contained mux (used by tests and standalone use).
// Production wires Routes into the gateway mux instead.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.Routes() {
		mux.Handle(r.Pattern, r.Handler)
	}
	return mux
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	var req sessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if req.SessionID == "" || req.RawTurns == "" {
		writeErr(w, http.StatusBadRequest, "session_id and raw_turns are required")
		return
	}
	// Kanban exclusion gate: boards listed in interestmemory.kanban_exclude
	// are dropped at the ingest boundary — before SaveTranscript, before the
	// worker queue, before any embedding, memory storage or token accounting.
	// The push is acknowledged (202) so the caller's dedupe state advances and
	// the session is not retried, but nothing is persisted or processed.
	if s.svc.KanbanBoardExcluded(req.KanbanBoard, req.KanbanBoardName) {
		writeJSON(w, http.StatusAccepted, map[string]string{
			"job_id":  "",
			"skipped": "kanban_board_excluded",
		})
		return
	}
	tx := store.Transcript{
		SessionID: req.SessionID,
		AgentID:   agent,
		TurnCount: req.TurnCount,
		RawTurns:  req.RawTurns,
	}
	// ReceivedAt is the server-side receive time.
	tx.ReceivedAt = timeNow()
	// SessionDate is the client-passed session start time (RFC3339, optional).
	if req.SessionDate != "" {
		if t, err := time.Parse(time.RFC3339, req.SessionDate); err == nil {
			utc := t.UTC()
			tx.SessionDate = &utc
		}
	}
	if err := s.svc.SaveTranscript(r.Context(), tx); err != nil {
		writeErr(w, http.StatusInternalServerError, "save transcript: "+err.Error())
		return
	}
	jobID, err := s.wk.Enqueue(r.Context(), agent, req.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	query := r.URL.Query().Get("query")
	if query == "" {
		writeErr(w, http.StatusBadRequest, "missing query")
		return
	}
	var opts recall.Options
	if v := r.URL.Query().Get("after"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			utc := t.UTC()
			opts.After = &utc
		}
	}
	if v := r.URL.Query().Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			utc := t.UTC()
			opts.Before = &utc
		}
	}
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.RecentDays = n
		}
	}
	ctxText, err := s.svc.Recall(r.Context(), agent, query, opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "recall: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"memory_context": ctxText})
}

func (s *Server) handleInterestPoints(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	pts, err := s.svc.ListInterestPoints(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list interest points: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": pts})
}

func (s *Server) handleWikiPages(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	pageType := store.PageType(r.URL.Query().Get("type"))
	pages, err := s.svc.ListPages(r.Context(), agent, pageType)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list pages: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": pages})
}

// handlePendingLinks serves dead-link feedback: [[target]] wikilinks whose
// target page does not exist yet.
func (s *Server) handlePendingLinks(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	links, err := s.svc.ListPendingLinks(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list pending links: "+err.Error())
		return
	}
	if links == nil {
		links = []store.PendingLink{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": links})
}

// handleGraph serves the full agent graph (nodes + edges) for visualization.
// Nodes are interest points + wiki pages (medium fields); edges are all five
// kinds, unfiltered — the frontend filters by kind/status.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	g, err := s.svc.ListGraph(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list graph: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// handleGraphHTML serves the embedded 3D visualization page. The page fetches
// /api/v1/{agent}/graph itself, resolving {agent} from the URL.
func (s *Server) handleGraphHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(graphHTML))
}

// handleSearch serves the consumer-side memory_search tool: ?query= does a
// semantic retrieval (structured hits incl. edges); ?id= fetches one entity
// by id. Requires one of them. id takes precedence when both are given.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	id := r.URL.Query().Get("id")
	query := r.URL.Query().Get("query")

	if id != "" {
		item, err := s.svc.GetByID(r.Context(), agent, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "get by id: "+err.Error())
			return
		}
		if item == nil {
			writeJSON(w, http.StatusOK, map[string]any{"items": []recall.Result{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": []recall.Result{*item}})
		return
	}
	if query == "" {
		writeErr(w, http.StatusBadRequest, "missing 'query' or 'id'")
		return
	}
	topK := 0
	if v := r.URL.Query().Get("top_k"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			topK = n
		}
	}
	items, err := s.svc.Search(r.Context(), agent, query, topK)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "search: "+err.Error())
		return
	}
	if items == nil {
		items = []recall.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleLogs serves change-log queries: ?limit=&offset= pagination (newest
// first). Default limit 50.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	logs, err := s.svc.ListLogs(r.Context(), agent, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list logs: "+err.Error())
		return
	}
	if logs == nil {
		logs = []store.ChangeLog{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": logs})
}

func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	tx, err := s.svc.ForkManual(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "fork: "+err.Error())
		return
	}
	if tx == nil {
		writeErr(w, http.StatusNotFound, "no unprocessed transcripts for agent")
		return
	}
	jobID, err := s.wk.Enqueue(r.Context(), agent, tx.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "session_id": tx.SessionID})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	jobID := r.PathValue("id")
	j, err := s.wk.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, worker.ErrNoJob) {
			writeErr(w, http.StatusNotFound, "job not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "get job: "+err.Error())
		return
	}
	// Namespace check: the {agent} path segment must match the job's agent.
	if j.AgentID != agent {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	stats, err := s.svc.Stats(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleUsage serves per-day token usage. ?since=YYYY-MM-DD returns days from
// that date onward (inclusive); omitted → all days.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	rows, err := s.svc.Usage(r.Context(), since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "usage: "+err.Error())
		return
	}
	if rows == nil {
		rows = []store.UsageRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": rows})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
