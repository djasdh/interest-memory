package service

import (
	"context"
	"fmt"

	"interest-memory/internal/config"
	"interest-memory/internal/fork"
	"interest-memory/internal/interest"
	"interest-memory/internal/llm"
	"interest-memory/internal/recall"
	"interest-memory/internal/store"
	"interest-memory/internal/transcript"
	"interest-memory/internal/vec"
	"interest-memory/internal/verify"
	"interest-memory/internal/wiki"

	"my-agent-core/provider"
	"my-agent-core/types"
)

// Service orchestrates the full memory pipeline (design §五): it wires the
// domain services together and exposes the operations the worker queue and
// HTTP layer call.
type Service struct {
	cfg     config.Config
	store   store.Store
	fork    fork.ForkAnalyzer
	verify  verify.Verifier
	interest interest.Cleaner
	wiki    wiki.Compiler
	recall  recall.RecallService
}

// New wires the domain services from config + infrastructure.
func New(
	cfg config.Config,
	st store.Store,
	vi vec.VectorIndex,
	llmClient *llm.Client,
	embedder *llm.Embedder,
) *Service {
	verifier := verify.New(llmClient, st, newWebSearchSearcher(cfg.Verify), verify.Config{
		UseWebSearch: cfg.Verify.UseWebSearch,
		SearchMax:    cfg.Verify.SearchMax,
		LLM:          cfg.LLM,
	})

	wikiDeps := wiki.ToolsDeps{Store: st, Vec: vi, Embedder: embedder}
	wikiProv := func(context.Context) (*provider.Provider, error) {
		return buildWikiProvider(cfg), nil
	}

	return &Service{
		cfg:      cfg,
		store:    st,
		fork:     fork.NewAnalyzer(llmClient, cfg.Fork),
		verify:   verifier,
		interest: interest.New(embedder, vi, st, cfg.Fork),
		wiki:     wiki.NewWriter(wikiDeps, wikiProv),
		recall:   recall.New(embedder, vi, st, verifier),
	}
}

// ProcessSession runs the full pipeline for one pushed transcript
// (design §五 steps 1-7). Returns an error that aborts the run; the caller
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

	// 2. verify#1: fact-check + evidence
	verified, err := s.verify.VerifyCandidates(ctx, agentID, cands)
	if err != nil {
		return fmt.Errorf("service: verify#1: %w", err)
	}

	// 3. interest: clean/dedup/merge/relate
	pts, err := s.interest.Clean(ctx, agentID, verified)
	if err != nil {
		return fmt.Errorf("service: interest: %w", err)
	}
	if len(pts) == 0 {
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

	// 5-6. wiki: compress + agent-loop write
	if err := s.wiki.Compile(ctx, agentID, pts, msgs); err != nil {
		return fmt.Errorf("service: wiki compile: %w", err)
	}
	// 7. rebuild adjacency from wikilinks
	if err := s.wiki.RebuildEdges(ctx, agentID); err != nil {
		return fmt.Errorf("service: rebuild edges: %w", err)
	}
	return nil
}

// persistContradictions stores detected contradictions and the bidirectional
// contradicts edges (design §五 step 4 → §四 contradictions table).
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
	}
	return nil
}

// Recall wraps the recall service with configured defaults.
func (s *Service) Recall(ctx context.Context, agentID, query string) (string, error) {
	return s.recall.Recall(ctx, agentID, query, recall.Options{
		TopK:        s.cfg.Recall.TopK,
		IncludeWiki: s.cfg.Recall.IncludeWiki,
		MinScore:    s.cfg.Recall.MinScore,
	})
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
