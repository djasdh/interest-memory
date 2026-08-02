package recall

import (
	"context"
	"fmt"
	"strings"

	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/verify"
)

// Embedder computes the query embedding (implemented by *llm.Embedder).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorIndex is the search surface (implemented by vec.SQLiteVec /
// vec.Fallback).
type VectorIndex interface {
	Search(ctx context.Context, agentID string, q []float32, topK int) ([]vec.Hit, error)
	SearchByKeywords(ctx context.Context, agentID, query string, topK int) ([]vec.Hit, error)
}

// Store loads hit entities for injection (implemented by *store.SQLiteStore).
type Store interface {
	GetInterestPoint(ctx context.Context, agentID, id string) (*store.InterestPoint, error)
	GetPage(ctx context.Context, agentID, id string) (*store.Page, error)
}

// Grader annotates recall hits (implemented by verify.Verifier).
type Grader interface {
	GradeForRecall(ctx context.Context, agentID string, hits []vec.Hit) ([]verify.Graded, error)
}

// Options controls one recall request (design §八 GET /recall).
type Options struct {
	TopK        int
	IncludeWiki bool
	MinScore    float64
}

// RecallService is the domain interface (design §七).
type RecallService interface {
	Recall(ctx context.Context, agentID, query string, opts Options) (string, error)
}

type service struct {
	embedder Embedder
	vec      VectorIndex
	store    Store
	grader   Grader
}

// New builds the recall service.
func New(embedder Embedder, vi VectorIndex, st Store, grader Grader) RecallService {
	return &service{embedder: embedder, vec: vi, store: st, grader: grader}
}

// Recall embeds the query, retrieves interest points + wiki pages by vector
// (keyword fallback), grades them for injection, and assembles a
// <memory-context> block (design §五: 会话始 GET /recall).
func (s *service) Recall(ctx context.Context, agentID, query string, opts Options) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", nil
	}
	if opts.TopK <= 0 {
		opts.TopK = 8
	}

	hits, err := s.retrieve(ctx, agentID, query, opts)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", nil
	}

	graded, err := s.grader.GradeForRecall(ctx, agentID, hits)
	if err != nil {
		return "", fmt.Errorf("recall: grade: %w", err)
	}

	return assemble(graded), nil
}

// retrieve runs vector search then keyword fallback, merging and re-ranking.
func (s *service) retrieve(ctx context.Context, agentID, query string, opts Options) ([]vec.Hit, error) {
	hits, err := s.vecSearch(ctx, agentID, query, opts.TopK)
	if err != nil || len(hits) == 0 {
		kw, kerr := s.vec.SearchByKeywords(ctx, agentID, query, opts.TopK)
		if kerr == nil && len(kw) > 0 {
			hits = kw
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}

	// Apply min-score threshold and (optionally) drop wiki pages.
	filtered := hits[:0]
	for _, h := range hits {
		if opts.MinScore > 0 && h.Score < float32(opts.MinScore) {
			continue
		}
		if !opts.IncludeWiki && h.Kind == "wiki_page" {
			continue
		}
		filtered = append(filtered, h)
	}
	if len(filtered) > opts.TopK {
		filtered = filtered[:opts.TopK]
	}
	return filtered, nil
}

func (s *service) vecSearch(ctx context.Context, agentID, query string, topK int) ([]vec.Hit, error) {
	if s.embedder == nil || s.vec == nil {
		return nil, nil
	}
	q, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.vec.Search(ctx, agentID, q, topK)
}

// assemble renders the recalled context as bare text. The Hermes plugin
// re-wraps it in <memory-context> (build_memory_context_block), so we must
// NOT emit the fence here (stage 6 decision: 服务端返回裸文本).
func assemble(graded []verify.Graded) string {
	var b strings.Builder
	for _, g := range graded {
		b.WriteString(renderOne(g))
	}
	return b.String()
}

func renderOne(g verify.Graded) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("- %s [%s]", titleOf(g), g.Hit.Kind))
	if g.Confidence > 0 {
		b.WriteString(fmt.Sprintf(" (confidence %.2f, %s)", g.Confidence, g.Status))
	}
	if g.FreshLevel != "" && g.FreshLevel != "unknown" {
		b.WriteString(fmt.Sprintf(" freshness=%s", g.FreshLevel))
	}
	if g.Note != "" {
		b.WriteString(" — " + g.Note)
	}
	b.WriteString("\n")
	return b.String()
}

func titleOf(g verify.Graded) string {
	if g.Title != "" {
		return g.Title
	}
	return g.Hit.ID
}

var _ RecallService = (*service)(nil)
