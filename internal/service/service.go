package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/fork"
	"github.com/djasdh/interest-memory/internal/interest"
	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/recall"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/transcript"
	"github.com/djasdh/interest-memory/internal/usage"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/verify"
	"github.com/djasdh/interest-memory/internal/websearch"
	"github.com/djasdh/interest-memory/internal/wiki"

	"github.com/djasdh/my-agent-core/mcpclient"
	"github.com/djasdh/my-agent-core/provider"
	"github.com/djasdh/my-agent-core/types"
)

// namespaceCacheTTL is how long the "all" namespace-sharing mode caches the
// discovered namespace list before re-reading the store.
const namespaceCacheTTL = 30 * time.Second

// Service orchestrates the full memory pipeline: it wires the
// domain services together and exposes the operations the worker queue and
// HTTP layer call.
type Service struct {
	cfg    config.Config
	store  store.Store
	fork   fork.ForkAnalyzer
	wiki   wiki.Compiler
	recall recall.RecallService
	usage  *usage.Tracker
	mcp    *mcpclient.Manager
	// embedder/llm drive the V1 unified-adjudication pipeline (s1→s2→V1.2→
	// V1.3). Stored as interfaces so ProcessSession tests can inject fakes.
	embedder interest.Embedder
	llm      interest.ClusterLLM
	// vec is the vector index used by s2 clustering (historical recall).
	vec vec.VectorIndex
}

// New wires the domain services from config + infrastructure. extraWebTools
// are optional additional network tools registered alongside the default
// "myagent" tool (selected via verify.web_tool).
func New(
	cfg config.Config,
	st store.Store,
	vi vec.VectorIndex,
	llmClient *llm.Client,
	embedder *llm.Embedder,
	extraWebTools ...websearch.Tool,
) *Service {
	reg := newWebSearchRegistry(cfg.Verify, extraWebTools...)
	// Optional MCP-backed web search: register an MCP tool alongside the
	// default "myagent" backend; verify.web_tool selects which is active.
	var mcpMgr *mcpclient.Manager
	if cfg.Verify.UseWebSearch && cfg.MCP.Enabled {
		if m := connectMCP(cfg.MCP); m != nil {
			mcpMgr = m
			_ = reg.Register(&mcpSearchTool{mgr: m, searchTool: cfg.MCP.SearchTool})
		}
	}
	verifier := verify.New(llmClient, st, reg, vi, embedder, verify.Config{
		UseWebSearch:   cfg.Verify.UseWebSearch,
		SearchMax:      cfg.Verify.SearchMax,
		WebTool:        cfg.Verify.WebTool,
		LLM:            cfg.LLM,
		MaxConcurrency: cfg.Verify.MaxConcurrency,
		Language:       cfg.Wiki.OutputLanguage(),
		MinConfidence:  cfg.Verify.MinConfidence,
		SimThreshold:   cfg.Verify.SimThreshold,
		MaxCandidates:  cfg.Verify.MaxCandidates,
	})

	wikiDeps := wiki.ToolsDeps{Store: st, Vec: vi, Embedder: embedder, Search: reg, LLM: llmClient}
	wikiProv := func(context.Context) (*provider.Provider, error) {
		return buildWikiProvider(cfg), nil
	}
	// Change-log retention default from config (0 = unlimited).
	_ = st.SetLogRetainDefault(context.Background(), cfg.Log.Retain)

	// Global token-usage tracker: deltas are aggregated in memory and flushed
	// to the store's per-day usage table every 5s (batching SQLite writes).
	tracker := usage.NewTracker(func(date string, u usage.Usage) error {
		return st.AddUsage(context.Background(), date, u.Input, u.Output, u.CacheHit)
	}, 5*time.Second)
	if llmClient != nil {
		llmClient.SetTracker(tracker)
	}
	if embedder != nil {
		embedder.SetTracker(tracker)
	}

	writer := wiki.NewWriter(wikiDeps, wikiProv, cfg.Wiki.OutputLanguage(), cfg.Wiki.VerifyClaims)
	writer.SetTracker(tracker)

	return &Service{
		cfg:      cfg,
		store:    st,
		fork:     fork.NewAnalyzer(llmClient, cfg.Fork, cfg.Wiki.Selective),
		wiki:     writer,
		recall:   recall.New(embedder, vi, st, verifier, buildNamespaceResolver(cfg, st)),
		usage:    tracker,
		mcp:      mcpMgr,
		embedder: embedder,
		llm:      llmClient,
		vec:      vi,
	}
}

// Close releases external resources (MCP server connections) and flushes any
// pending token-usage deltas. Safe to call once.
func (s *Service) Close() {
	if s.usage != nil {
		s.usage.Close()
	}
	if s.mcp != nil {
		s.mcp.Close()
		s.mcp = nil
	}
}

// connectMCP parses config, connects to each MCP server, and returns the
// manager (or nil on failure). Per-server connection errors are logged but
// do not abort other servers.
func connectMCP(cfg config.MCPConfig) *mcpclient.Manager {
	configs, err := mcpclient.ParseConfig(cfg.Servers)
	if err != nil {
		log.Printf("mcp: parse servers: %v", err)
		return nil
	}
	if len(configs) == 0 {
		return nil
	}
	mgr := mcpclient.NewManager(configs)
	for _, e := range mgr.ConnectAll(context.Background()) {
		log.Printf("mcp: connect: %v", e)
	}
	return mgr
}

// ProcessSession runs the full pipeline for one pushed transcript
// (steps 1-7). Returns an error that aborts the run; the caller
// decides whether to mark the transcript processed.
func (s *Service) ProcessSession(ctx context.Context, agentID string, t store.Transcript) error {
	msgs, err := transcript.ToMessages(t.RawTurns)
	if err != nil {
		return fmt.Errorf("service: transcript parse: %w", err)
	}
	if len(msgs) == 0 {
		// Nothing worth remembering — still mark processed downstream.
		return nil
	}

	// 1. fork: prefix-window split + concurrent side-LLM extraction
	windows := fork.SplitPrefixWindows(toLLMMessages(msgs), s.cfg.Fork.PrefixStep, s.cfg.Fork.MaxWindows)
	cands, err := s.fork.Analyze(ctx, agentID, windows)
	if err != nil {
		return fmt.Errorf("service: fork: %w", err)
	}
	if len(cands) == 0 {
		return nil
	}
	s.setCandidateEventTime(cands, t.SessionDate, t.ReceivedAt)

	// 2-5. V1 unified adjudication pipeline:
	//   s1 dedupe-merge → s2 cluster → V1.2 adjudicate → V1.3 persist.
	// Short-circuit when embedder/llm are absent (e.g. degraded startup) or
	// the pipeline produces nothing to persist.
	if s.embedder == nil || s.llm == nil || s.vec == nil {
		return nil
	}
	pts, err := interest.DedupeMerge(ctx, agentID, s.embedder, s.llm, s.cfg.Fork.ClusterSim, cands)
	if err != nil {
		return fmt.Errorf("service: dedupe-merge: %w", err)
	}
	if len(pts) == 0 {
		return nil
	}
	res, err := interest.Cluster(ctx, agentID, s.vec, s.store, pts, s.cfg.Fork.SimilarityMerge, s.cfg.Fork.HistSim)
	if err != nil {
		return fmt.Errorf("service: cluster: %w", err)
	}
	adj, err := interest.Adjudicate(ctx, agentID, s.embedder, s.llm, res, s.cfg.Fork.MaxConcurrency)
	if err != nil {
		return fmt.Errorf("service: adjudicate: %w", err)
	}
	if err := interest.Persist(ctx, agentID, s.store, s.vec, adj, s.cfg.Fork.SimilarityRelate); err != nil {
		return fmt.Errorf("service: persist: %w", err)
	}
	if len(adj.FinalPoints) == 0 && len(adj.Archived) == 0 {
		return nil
	}

	// Extract final interest points (create/update) for the wiki stage and
	// archived historical point ids for reconcile. WikiWorthy was already
	// decided by V1.2 adjudication, so no selective re-filtering is needed.
	var finalPts []store.InterestPoint
	for _, fp := range adj.FinalPoints {
		if fp.Action == "create" || fp.Action == "update" {
			finalPts = append(finalPts, fp.Point)
		}
	}
	var archived []string
	for _, ap := range adj.Archived {
		archived = append(archived, ap.Pt.ID)
	}
	if len(finalPts) == 0 && len(archived) == 0 {
		return nil
	}

	// 5-8. wiki: per-interest-point agent-loop write + adjacency rebuild +
	// related-page reconcile. Skipped entirely when wiki writing is disabled
	// (config.Wiki.Enabled=false) — interest points are still persisted.
	var touched []string
	if s.cfg.Wiki.Enabled {
		touched, err = s.wiki.Compile(ctx, agentID, finalPts, msgs)
		if err != nil {
			return fmt.Errorf("service: wiki compile: %w", err)
		}
		// 7. rebuild adjacency from wikilinks (incremental: touched pages only)
		if err := s.wiki.RebuildEdges(ctx, agentID, touched); err != nil {
			return fmt.Errorf("service: rebuild edges: %w", err)
		}
		// 8. reconcile related pages: propagate structural changes (page
		// writes + archived interest points) to related pages within
		// max_hops, batched.
		if err := s.wiki.ReconcileRelated(ctx, agentID, wiki.ReconcileInput{
			TouchedPages:   touched,
			ArchivedPoints: archived,
		}, s.cfg.Wiki.MaxHops, s.cfg.Wiki.BatchSize); err != nil {
			return fmt.Errorf("service: reconcile: %w", err)
		}
	}
	return nil
}

// buildNamespaceResolver translates the configured namespace sharing into a
// recall resolver. isolated (default) → nil (each agent reads only its own
// namespace); all → discover every persisted namespace dynamically; custom →
// the per-agent visible_to map (one-way visibility). nil disables annotation.
func buildNamespaceResolver(cfg config.Config, st store.Store) recall.NamespaceResolver {
	switch cfg.Namespaces.Mode {
	case config.NamespaceAll:
		var (
			mu      sync.Mutex
			cached  []string
			expires time.Time
		)
		return func(ctx context.Context, agentID string) ([]string, error) {
			mu.Lock()
			if cached != nil && time.Now().Before(expires) {
				ids := cached
				mu.Unlock()
				return ids, nil
			}
			mu.Unlock()
			ids, err := st.ListAgentIDs(ctx)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			cached = ids
			expires = time.Now().Add(namespaceCacheTTL)
			mu.Unlock()
			return ids, nil
		}
	case config.NamespaceCustom:
		return func(ctx context.Context, agentID string) ([]string, error) {
			return cfg.Namespaces.VisibleTo[agentID], nil
		}
	default:
		return nil
	}
}

// ListLogs returns change logs for an agent, newest first, paginated.
func (s *Service) ListLogs(ctx context.Context, agentID string, limit, offset int) ([]store.ChangeLog, error) {
	return s.store.ListLogs(ctx, agentID, limit, offset)
}

// Recall wraps the recall service, filling configured defaults for fields
// the caller did not set (topK/includeWiki/minScore, temporal filters passthrough).
func (s *Service) Recall(ctx context.Context, agentID, query string, opts recall.Options) (string, error) {
	if opts.TopK <= 0 {
		opts.TopK = s.cfg.Recall.TopK
	}
	if !s.cfg.Wiki.Enabled {
		// Wiki pages aren't written when the wiki stage is disabled — never
		// include them in recall.
		opts.IncludeWiki = false
	} else if !opts.IncludeWiki {
		opts.IncludeWiki = s.cfg.Recall.IncludeWiki
	}
	if opts.MinScore <= 0 {
		opts.MinScore = s.cfg.Recall.MinScore
	}
	return s.recall.Recall(ctx, agentID, query, opts)
}

// Search is the consumer-side memory_search: structured hits with full
// content + edges (see recall.Result). topK<=0 falls back to config. Wiki
// pages are filtered out when the wiki stage is disabled.
func (s *Service) Search(ctx context.Context, agentID, query string, topK int) ([]recall.Result, error) {
	if topK <= 0 {
		topK = s.cfg.Search.TopK
	}
	items, err := s.recall.Search(ctx, agentID, query, topK, s.cfg.Search.MaxBodyLen)
	if err != nil {
		return nil, err
	}
	if s.cfg.Wiki.Enabled {
		return items, nil
	}
	filtered := items[:0]
	for _, it := range items {
		if it.Kind == "wiki_page" {
			continue
		}
		filtered = append(filtered, it)
	}
	return filtered, nil
}

// GetByID fetches one entity (page or interest point) with full content +
// edges for the consumer-side memory_search id lookup. Wiki pages resolve to
// nil when the wiki stage is disabled.
func (s *Service) GetByID(ctx context.Context, agentID, id string) (*recall.Result, error) {
	r, err := s.recall.GetByID(ctx, agentID, id, s.cfg.Search.MaxBodyLen)
	if err != nil {
		return nil, err
	}
	if r != nil && !s.cfg.Wiki.Enabled && r.Kind == "wiki_page" {
		return nil, nil
	}
	return r, nil
}

// SaveTranscript persists a pushed session-end transcript.
func (s *Service) SaveTranscript(ctx context.Context, t store.Transcript) error {
	return s.store.SaveTranscript(ctx, t)
}

// ListInterestPoints returns all interest points for an agent.
func (s *Service) ListInterestPoints(ctx context.Context, agentID string) ([]store.InterestPoint, error) {
	return s.store.ListInterestPoints(ctx, agentID)
}

// ListPages returns wiki pages for an agent, optionally filtered by type.
func (s *Service) ListPages(ctx context.Context, agentID string, pageType store.PageType) ([]store.Page, error) {
	return s.store.ListPages(ctx, agentID, pageType)
}

// ListPendingLinks returns dead-link records for an agent.
func (s *Service) ListPendingLinks(ctx context.Context, agentID string) ([]store.PendingLink, error) {
	return s.store.ListPendingLinks(ctx, agentID)
}

// Stats returns simple counts for an agent.
func (s *Service) Stats(ctx context.Context, agentID string) (map[string]int, error) {
	ips, err := s.store.CountInterestPoints(ctx, agentID)
	if err != nil {
		return nil, err
	}
	pages, err := s.store.CountPages(ctx, agentID)
	if err != nil {
		return nil, err
	}
	cons, err := s.store.CountContradictions(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return map[string]int{
		"interest_points": ips,
		"wiki_pages":      pages,
		"contradictions":  cons,
	}, nil
}

// Usage returns per-day token usage from (inclusive) a date onward (YYYY-MM-DD).
// An empty since returns all days, oldest first.
func (s *Service) Usage(ctx context.Context, since string) ([]store.UsageRow, error) {
	return s.store.ListUsage(ctx, since)
}

// ForkManual is invoked by POST /fork: pull the oldest unprocessed
// transcript and return it (the worker will process it).
func (s *Service) ForkManual(ctx context.Context, agentID string) (*store.Transcript, error) {
	list, err := s.store.ListUnprocessedTranscripts(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// setCandidateEventTime stamps extracted candidates with the event time:
// the passed session start date when present, otherwise the server receive
// time (fallback guarantees a non-zero EventTime for temporal filtering).
func (s *Service) setCandidateEventTime(cands []fork.Candidate, sessionDate *time.Time, receivedAt time.Time) {
	et := receivedAt
	if sessionDate != nil {
		et = *sessionDate
	}
	for i := range cands {
		cands[i].EventTime = et
	}
}

// toLLMMessages maps my-agent-core messages to the internal llm.Message
// shape used by the fork analyzer.
func toLLMMessages(msgs []types.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		switch m.Role {
		case types.RoleAssistant:
			role = "assistant"
		case types.RoleToolResult:
			role = "tool"
		}
		out = append(out, llm.Message{Role: role, Content: m.Text})
	}
	return out
}
