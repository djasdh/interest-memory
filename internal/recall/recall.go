package recall

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	Outlinks(ctx context.Context, agentID, sourceID string) ([]store.Edge, error)
	Backlinks(ctx context.Context, agentID, targetID string) ([]store.Edge, error)
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
	// After/Before are RFC3339 event-time filters (EventTime must fall in
	// range). RecentDays filters to the last N days (After = now - N*24h).
	After      *time.Time
	Before     *time.Time
	RecentDays int
}

// RecallService is the domain interface (design §七).
type RecallService interface {
	Recall(ctx context.Context, agentID, query string, opts Options) (string, error)
	// Search returns structured hits (full body/claims/evidence + edges) for
	// the consumer-side memory_search tool. body is truncated to maxBodyLen.
	Search(ctx context.Context, agentID, query string, topK, maxBodyLen int) ([]Result, error)
	// GetByID returns one entity (page or interest point) with full content +
	// edges, or nil when the id is unknown.
	GetByID(ctx context.Context, agentID, id string, maxBodyLen int) (*Result, error)
}

// EdgeRef is an adjacency edge projected for the consumer tool: the far-end
// id + title (page Title or interest point Name), without the far-end body.
type EdgeRef struct {
	ID     string          `json:"id"`
	Title  string          `json:"title"`
	Kind   store.EdgeType  `json:"kind"`
	Weight float64         `json:"weight"`
}

// Result is one structured search hit for the consumer-side memory_search
// tool: full content plus adjacency edges (with target titles).
type Result struct {
	Kind        string            `json:"kind"` // interest_point | wiki_page
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	BodyMD      string            `json:"body_md"` // 页正文或兴趣点摘要（按 max_body_len 截断）
	Status      string            `json:"status"`
	Subjective  bool              `json:"subjective,omitempty"`
	Claims      []store.Claim     `json:"claims,omitempty"`
	Evidence    []store.Evidence  `json:"evidence,omitempty"`
	Reliability store.Reliability `json:"reliability"`
	Freshness   store.Freshness   `json:"freshness"`
	Outlinks    []EdgeRef         `json:"outlinks"`
	Backlinks   []EdgeRef         `json:"backlinks"`
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

	// Event-time window from options.
	after, before := opts.After, opts.Before
	if opts.RecentDays > 0 {
		t := time.Now().UTC().AddDate(0, 0, -opts.RecentDays)
		if after == nil || t.After(*after) {
			after = &t
		}
	}

	// Apply min-score threshold, (optionally) drop wiki pages, and filter by
	// event time.
	filtered := hits[:0]
	for _, h := range hits {
		if opts.MinScore > 0 && h.Score < float32(opts.MinScore) {
			continue
		}
		if !opts.IncludeWiki && h.Kind == "wiki_page" {
			continue
		}
		if after != nil || before != nil {
			et := s.eventTimeOf(ctx, agentID, h)
			if after != nil && (et.IsZero() || et.Before(*after)) {
				continue
			}
			if before != nil && (et.IsZero() || et.After(*before)) {
				continue
			}
		}
		filtered = append(filtered, h)
	}
	if len(filtered) > opts.TopK {
		filtered = filtered[:opts.TopK]
	}
	return filtered, nil
}

// eventTimeOf resolves an entity's EventTime for temporal filtering.
func (s *service) eventTimeOf(ctx context.Context, agentID string, h vec.Hit) time.Time {
	if h.Kind == "interest_point" {
		if p, err := s.store.GetInterestPoint(ctx, agentID, h.ID); err == nil && p != nil {
			return p.EventTime
		}
	}
	if pg, err := s.store.GetPage(ctx, agentID, h.ID); err == nil && pg != nil {
		return pg.EventTime
	}
	return time.Time{}
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
	if !g.EventTime.IsZero() {
		b.WriteString(fmt.Sprintf(" (at %s)", g.EventTime.UTC().Format("2006-01-02")))
	}
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

// Search returns structured hits (full content + edges) for the
// consumer-side memory_search tool. Falls back to keyword search when
// embedding/vector search is unavailable; returns empty on failure.
func (s *service) Search(ctx context.Context, agentID, query string, topK, maxBodyLen int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = 3
	}
	var hits []vec.Hit
	if s.embedder != nil && s.vec != nil {
		q, err := s.embedder.Embed(ctx, query)
		if err == nil {
			hits, _ = s.vec.Search(ctx, agentID, q, topK)
		}
	}
	if len(hits) == 0 && s.vec != nil {
		kw, kerr := s.vec.SearchByKeywords(ctx, agentID, query, topK)
		if kerr == nil && len(kw) > 0 {
			hits = kw
		}
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	var out []Result
	for _, h := range hits {
		r, err := s.resultFor(ctx, agentID, h, maxBodyLen)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// GetByID returns one entity with full content + edges, or nil when unknown.
func (s *service) GetByID(ctx context.Context, agentID, id string, maxBodyLen int) (*Result, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	if p, err := s.store.GetInterestPoint(ctx, agentID, id); err == nil && p != nil {
		return s.resultFor(ctx, agentID, vec.Hit{ID: id, Kind: "interest_point"}, maxBodyLen)
	}
	pg, err := s.store.GetPage(ctx, agentID, id)
	if err != nil || pg == nil {
		return nil, nil
	}
	return s.resultFor(ctx, agentID, vec.Hit{ID: id, Kind: "wiki_page"}, maxBodyLen)
}

// resultFor assembles one Result for a hit, attaching edges with far-end
// titles. Archived/superseded entities are filtered out.
func (s *service) resultFor(ctx context.Context, agentID string, h vec.Hit, maxBodyLen int) (*Result, error) {
	if h.Kind == "interest_point" {
		p, err := s.store.GetInterestPoint(ctx, agentID, h.ID)
		if err != nil || p == nil {
			return nil, nil
		}
		if p.Status == "archived" {
			return nil, nil
		}
		r := &Result{
			Kind:        "interest_point",
			ID:          p.ID,
			Title:       p.Name,
			BodyMD:      truncate(p.Summary, maxBodyLen),
			Status:      p.Status,
			Subjective:  p.Subjective,
			Evidence:    p.Reliability.Evidence,
			Reliability: p.Reliability,
			Freshness:   p.Freshness,
		}
		s.attachEdges(ctx, agentID, h.ID, r)
		return r, nil
	}
	pg, err := s.store.GetPage(ctx, agentID, h.ID)
	if err != nil || pg == nil {
		return nil, nil
	}
	if pg.Status != "" && pg.Status != "active" {
		return nil, nil
	}
	r := &Result{
		Kind:        "wiki_page",
		ID:          pg.ID,
		Title:       pg.Title,
		BodyMD:      truncate(pg.BodyMD, maxBodyLen),
		Status:      pg.Status,
		Claims:      pg.Claims,
		Freshness:   store.Freshness{Level: "unknown"},
	}
	s.attachEdges(ctx, agentID, h.ID, r)
	return r, nil
}

// attachEdges fills Outlinks/Backlinks, resolving the far-end title (page
// Title or interest point Name) for each edge.
func (s *service) attachEdges(ctx context.Context, agentID, id string, r *Result) {
	if outs, err := s.store.Outlinks(ctx, agentID, id); err == nil {
		for _, e := range outs {
			r.Outlinks = append(r.Outlinks, s.edgeRef(ctx, agentID, e, e.TargetID))
		}
	}
	if ins, err := s.store.Backlinks(ctx, agentID, id); err == nil {
		for _, e := range ins {
			r.Backlinks = append(r.Backlinks, s.edgeRef(ctx, agentID, e, e.SourceID))
		}
	}
}

// edgeRef resolves the far-end id's title for an edge.
func (s *service) edgeRef(ctx context.Context, agentID string, e store.Edge, farID string) EdgeRef {
	ref := EdgeRef{ID: farID, Kind: e.Kind, Weight: e.Weight}
	if p, err := s.store.GetInterestPoint(ctx, agentID, farID); err == nil && p != nil {
		ref.Title = p.Name
		return ref
	}
	if pg, err := s.store.GetPage(ctx, agentID, farID); err == nil && pg != nil {
		ref.Title = pg.Title
	}
	return ref
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ RecallService = (*service)(nil)
