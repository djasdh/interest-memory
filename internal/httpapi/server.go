package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"interest-memory/internal/recall"
	"interest-memory/internal/store"
	"interest-memory/internal/worker"
)

// Service is the orchestration surface the HTTP layer calls (implemented by
// *service.Service).
type Service interface {
	ProcessSession(ctx context.Context, agentID string, t store.Transcript) error
	SaveTranscript(ctx context.Context, t store.Transcript) error
	Recall(ctx context.Context, agentID, query string) (string, error)
	Search(ctx context.Context, agentID, query string) ([]recall.Result, error)
	GetByID(ctx context.Context, agentID, id string) (*recall.Result, error)
	ListInterestPoints(ctx context.Context, agentID string) ([]store.InterestPoint, error)
	ListPages(ctx context.Context, agentID string, pageType store.PageType) ([]store.Page, error)
	Stats(ctx context.Context, agentID string) (map[string]int, error)
	ForkManual(ctx context.Context, agentID string) (*store.Transcript, error)
}

// Worker is the async queue surface (implemented by *worker.Worker).
type Worker interface {
	Enqueue(ctx context.Context, agentID, sessionID string) (string, error)
	GetJob(ctx context.Context, jobID string) (*worker.Job, error)
}

// Server is the REST API server (design §八).
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
		{Pattern: "GET /api/v1/{agent}/search", Handler: s.handleSearch},
		{Pattern: "POST /api/v1/{agent}/fork", Handler: s.handleFork},
		{Pattern: "GET /api/v1/{agent}/jobs/{id}", Handler: s.handleJob},
		{Pattern: "GET /api/v1/{agent}/stats", Handler: s.handleStats},
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
	tx := store.Transcript{
		SessionID: req.SessionID,
		AgentID:   agent,
		TurnCount: req.TurnCount,
		RawTurns:  req.RawTurns,
	}
	// ReceivedAt is set by SaveTranscript? No — fill here.
	tx.ReceivedAt = timeNow()
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
	ctxText, err := s.svc.Recall(r.Context(), agent, query)
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
	items, err := s.svc.Search(r.Context(), agent, query)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "search: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
