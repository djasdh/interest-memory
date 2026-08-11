package service

import (
	"context"
	"fmt"
	"time"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/fork"
	"github.com/djasdh/interest-memory/internal/interest"
	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/recall"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/transcript"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/verify"
	"github.com/djasdh/interest-memory/internal/websearch"
	"github.com/djasdh/interest-memory/internal/wiki"

	"github.com/djasdh/my-agent-core/provider"
	"github.com/djasdh/my-agent-core/types"
)

// Service orchestrates the full memory pipeline: it wires the
// domain services together and exposes the operations the worker queue and
// HTTP layer call.
type Service struct {
	cfg      config.Config
	store    store.Store
	fork     fork.ForkAnalyzer
	verify   verify.Verifier
	interest interest.Cleaner
	wiki     wiki.Compiler
	recall   recall.RecallService
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

	return &Service{
		cfg:      cfg,
		store:    st,
		fork:     fork.NewAnalyzer(llmClient, cfg.Fork, cfg.Wiki.Selective),
		verify:   verifier,
		interest: interest.New(embedder, vi, st, cfg.Fork),
		wiki:     wiki.NewWriter(wikiDeps, wikiProv, cfg.Wiki.OutputLanguage()),
		recall:   recall.New(embedder, vi, st, verifier, buildNamespaceResolver(cfg, st)),
	}
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

	// 2. verify#1: fact-check + evidence
	verified, err := s.verify.VerifyCandidates(ctx, agentID, cands)
	if err != nil {
		return fmt.Errorf("service: verify#1: %w", err)
	}

	// 3. interest: clean/dedup/merge/relate (archived ids feed reconcile)
	pts, archived, err := s.interest.Clean(ctx, agentID, verified)
	if err != nil {
		return fmt.Errorf("service: interest: %w", err)
	}
	// Continue when only deletions happened: archived points still need to
	// reach the reconcile stage (cascade-archive related pages). Pure no-op
	// runs (nothing persisted, nothing archived) return early.
	if len(pts) == 0 && len(archived) == 0 {
		return nil
	}

	// 4. verify#2: claims + contradictions (contradictions persisted)
	claims, err := s.verify.CheckClaims(ctx, agentID, pts)
	if err != nil {
		return fmt.Errorf("service: check claims: %w", err)
	}
	if err := s.persistContradictions(ctx, agentID, pts, claims); err != nil {
		return fmt.Errorf("service: flag contradictions: %w", err)
	}

	// 5-8. wiki: per-interest-point agent-loop write + adjacency rebuild +
	// related-page reconcile. Skipped entirely when wiki writing is disabled
	// (config.Wiki.Enabled=false) — interest points and verify#2
	// contradictions are still persisted.
	var touched []string
	if s.cfg.Wiki.Enabled {
		touched, err = s.wiki.Compile(ctx, agentID, pts, msgs)
		if err != nil {
			return fmt.Errorf("service: wiki compile: %w", err)
		}
		// 7. rebuild adjacency from wikilinks
		if err := s.wiki.RebuildEdges(ctx, agentID); err != nil {
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

// persistContradictions stores detected contradictions and the bidirectional
// contradicts edges (pipeline step 4, stored in the contradictions table).
func (s *Service) persistContradictions(ctx context.Context, agentID string, pts []store.InterestPoint, claims []store.Claim) error {
	if len(claims) == 0 {
		return nil
	}
	cons, err := s.verify.FlagContradictions(ctx, agentID, claims)
	if err != nil {
		return err
	}
	for _, c := range cons {
		if err := s.store.UpsertContradiction(ctx, c); err != nil {
			return err
		}
		if err := s.store.AddEdgePair(ctx, agentID, store.Edge{
			SourceID: c.LeftID,
			TargetID: c.RightID,
			Kind:     store.EdgeContradict,
			Weight:   1,
		}); err != nil {
			return err
		}
		_ = s.store.AppendLog(ctx, store.ChangeLog{
			AgentID: agentID, EntityKind: "wiki_page", Action: "edge_change",
			Edges: []store.LogEdge{
				{Action: "add", SourceID: c.LeftID, TargetID: c.RightID, Kind: store.EdgeContradict, Weight: 1},
				{Action: "add", SourceID: c.RightID, TargetID: c.LeftID, Kind: store.EdgeContradict, Weight: 1},
			},
		})
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
		return func(ctx context.Context, agentID string) ([]string, error) {
			ids, err := st.ListAgentIDs(ctx)
			if err != nil {
				return nil, err
			}
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
	ips, err := s.store.ListInterestPoints(ctx, agentID)
	if err != nil {
		return nil, err
	}
	pages, err := s.store.ListPages(ctx, agentID, "")
	if err != nil {
		return nil, err
	}
	cons, err := s.store.ListContradictions(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return map[string]int{
		"interest_points": len(ips),
		"wiki_pages":      len(pages),
		"contradictions":  len(cons),
	}, nil
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
