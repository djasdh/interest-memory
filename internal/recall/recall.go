package recall

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/djasdh/interest-memory/internal/cache"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/verify"
)

// recallCacheTTL bounds how long an assembled recall context is reused. The
// store may change between calls; a short TTL keeps results fresh.
const recallCacheTTL = 30 * time.Second

type recallCacheEntry struct {
	text    string
	expires time.Time
}

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
	// GetInterestPointsByIDs fetches interest points by id in one query.
	GetInterestPointsByIDs(ctx context.Context, agentID string, ids []string) ([]store.InterestPoint, error)
	GetPage(ctx context.Context, agentID, id string) (*store.Page, error)
	// GetPagesByIDs fetches pages by id in one query.
	GetPagesByIDs(ctx context.Context, agentID string, ids []string) ([]store.Page, error)
	// ListClaims fetches a page's claims (GetPage no longer loads them).
	ListClaims(ctx context.Context, agentID, pageID string) ([]store.Claim, error)
	Outlinks(ctx context.Context, agentID, sourceID string) ([]store.Edge, error)
	Backlinks(ctx context.Context, agentID, targetID string) ([]store.Edge, error)
	// ResolveReplacement returns the live successor of an archived/superseded
	// entity (or nil) so search can silently substitute it.
	ResolveReplacement(ctx context.Context, agentID, id string) (*store.Replacement, error)
	// Full-text fallback: LIKE keyword search over stored entities. Used when
	// vector search and the vector index's own keyword path both come up
	// empty (SQLiteVec.SearchByKeywords is a no-op).
	SearchInterestPointsByKeywords(ctx context.Context, agentID, query string, limit int) ([]store.InterestPoint, error)
	SearchPagesByKeywords(ctx context.Context, agentID, query string, limit int) ([]store.Page, error)
}

// Grader annotates recall hits (implemented by verify.Verifier).
type Grader interface {
	GradeForRecall(ctx context.Context, agentID string, hits []vec.Hit) ([]verify.Graded, error)
}

// Options controls one recall request (GET /recall).
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

// RecallService is the domain interface.
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
	ID     string         `json:"id"`
	Title  string         `json:"title"`
	Kind   store.EdgeType `json:"kind"`
	Weight float64        `json:"weight"`
}

// Result is one structured search hit for the consumer-side memory_search
// tool: full content plus adjacency edges (with target titles).
type Result struct {
	Kind        string            `json:"kind"` // interest_point | wiki_page
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	BodyMD      string            `json:"body_md"` // page body or interest-point summary (truncated to max_body_len)
	Status      string            `json:"status"`
	Subjective  bool              `json:"subjective,omitempty"`
	Agent       string            `json:"agent,omitempty"` // source namespace (annotated in cross-namespace mode; empty when isolated)
	Claims      []store.Claim     `json:"claims,omitempty"`
	Evidence    []store.Evidence  `json:"evidence,omitempty"`
	Reliability store.Reliability `json:"reliability"`
	Freshness   store.Freshness   `json:"freshness"`
	Outlinks    []EdgeRef         `json:"outlinks"`
	Backlinks   []EdgeRef         `json:"backlinks"`
	// Replacement describes the live successor of an archived/superseded
	// entity (nil when the entity is current). Populated by GetByID so the
	// consumer can confirm what superseded this one.
	Replacement *ReplacementRef `json:"replacement,omitempty"`
}

// ReplacementRef projects a successor for an archived/superseded entity.
type ReplacementRef struct {
	Kind   string `json:"kind"` // interest_point | wiki_page
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type service struct {
	embedder Embedder
	vec      VectorIndex
	store    Store
	grader   Grader
	// resolver decides which namespaces one read request may see (the agent's
	// own namespace included). nil → isolated (self only). When set, results
	// are annotated with their source namespace.
	resolver NamespaceResolver
	annotate bool
	// recallCache short-TTL memo of assembled recall contexts, keyed by
	// agent+query+opts; guards against repeated embed/search calls.
	recallCache   *cache.Cache[string, recallCacheEntry]
	recallCacheMu sync.Mutex
}

// NamespaceResolver returns the namespaces an agent may read for a request
// (the agent's own namespace included). It is resolved per request so the set
// can be dynamic (e.g. the "all" mode discovers namespaces from the store).
type NamespaceResolver func(ctx context.Context, agentID string) ([]string, error)

// entityCache dedupes per-request entity loads so repeated hits on the same
// entity (Search across namespaces, edge title resolution) read the store once.
type entityCache struct {
	ips    map[string]*store.InterestPoint
	pgs    map[string]*store.Page
	titles map[string]string
}

func newEntityCache() *entityCache {
	return &entityCache{ips: map[string]*store.InterestPoint{}, pgs: map[string]*store.Page{}, titles: map[string]string{}}
}

// interestPoint loads an interest point once per request, reusing the cache on
// repeat ids.
func (ec *entityCache) interestPoint(ctx context.Context, agentID, id string, st Store) (*store.InterestPoint, error) {
	if p, ok := ec.ips[id]; ok {
		return p, nil
	}
	p, err := st.GetInterestPoint(ctx, agentID, id)
	if err != nil {
		return nil, err
	}
	if p != nil {
		ec.ips[id] = p
	}
	return p, nil
}

// page loads a wiki page once per request, reusing the cache on repeat ids.
func (ec *entityCache) page(ctx context.Context, agentID, id string, st Store) (*store.Page, error) {
	if p, ok := ec.pgs[id]; ok {
		return p, nil
	}
	p, err := st.GetPage(ctx, agentID, id)
	if err != nil {
		return nil, err
	}
	if p != nil {
		ec.pgs[id] = p
	}
	return p, nil
}

// New builds the recall service. Pass an optional NamespaceResolver to enable
// cross-namespace reads (results get annotated with their source namespace);
// with no resolver the service reads only the agent's own namespace (isolated).
func New(embedder Embedder, vi VectorIndex, st Store, grader Grader, resolvers ...NamespaceResolver) RecallService {
	s := &service{embedder: embedder, vec: vi, store: st, grader: grader,
		recallCache: cache.New[string, recallCacheEntry](512),
	}
	if len(resolvers) > 0 && resolvers[0] != nil {
		s.resolver = resolvers[0]
		s.annotate = true
	}
	return s
}

// visible returns the ordered set of namespaces to read for agentID: its own
// namespace first, then the resolver-provided ones (deduped).
func (s *service) visible(ctx context.Context, agentID string) []string {
	if s.resolver == nil {
		return []string{agentID}
	}
	ns, err := s.resolver(ctx, agentID)
	if err != nil || len(ns) == 0 {
		return []string{agentID}
	}
	seen := map[string]bool{agentID: true}
	out := []string{agentID}
	for _, n := range ns {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// dedupeHits drops duplicate (agent, id) pairs, keeping the first occurrence
// (callers sort by score before deduping so the highest score survives).
func dedupeHits(hits []vec.Hit) []vec.Hit {
	seen := map[string]bool{}
	out := hits[:0]
	for _, h := range hits {
		key := h.AgentID + "\x00" + h.ID
		if !seen[key] {
			seen[key] = true
			out = append(out, h)
		}
	}
	return out
}

// gradeByAgent grades hits grouped by their source agent (the grader is
// single-namespace scoped).
func (s *service) gradeByAgent(ctx context.Context, hits []vec.Hit) ([]verify.Graded, error) {
	byAgent := map[string][]vec.Hit{}
	var order []string
	for _, h := range hits {
		key := h.AgentID
		if key == "" {
			key = "\x00"
		}
		if _, ok := byAgent[key]; !ok {
			order = append(order, key)
		}
		byAgent[key] = append(byAgent[key], h)
	}
	var graded []verify.Graded
	for _, key := range order {
		agent := key
		if key == "\x00" {
			agent = ""
		}
		gs, err := s.grader.GradeForRecall(ctx, agent, byAgent[key])
		if err != nil {
			return nil, fmt.Errorf("recall: grade: %w", err)
		}
		graded = append(graded, gs...)
	}
	return graded, nil
}

// Recall embeds the query, retrieves interest points + wiki pages by vector
// (keyword fallback), grades them for injection, and assembles a
// <memory-context> block (session-start GET /recall).
func (s *service) Recall(ctx context.Context, agentID, query string, opts Options) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", nil
	}
	if opts.TopK <= 0 {
		opts.TopK = 8
	}

	key := recallCacheKey(agentID, query, opts)
	s.recallCacheMu.Lock()
	if e, ok := s.recallCache.Get(key); ok && time.Now().Before(e.expires) {
		s.recallCacheMu.Unlock()
		return e.text, nil
	}
	s.recallCacheMu.Unlock()

	// Retrieve across every visible namespace, then merge (dedupe by
	// agent+id), re-rank globally and cap to TopK.
	var all []vec.Hit
	for _, ns := range s.visible(ctx, agentID) {
		hits, err := s.retrieve(ctx, ns, query, opts)
		if err != nil {
			return "", err
		}
		all = append(all, hits...)
	}
	if len(all) == 0 {
		return "", nil
	}
	all = dedupeHits(all)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if len(all) > opts.TopK {
		all = all[:opts.TopK]
	}

	graded, err := s.gradeByAgent(ctx, all)
	if err != nil {
		return "", err
	}

	ctxText := s.assembleContext(graded)
	s.recallCacheMu.Lock()
	s.recallCache.Set(key, recallCacheEntry{text: ctxText, expires: time.Now().Add(recallCacheTTL)})
	s.recallCacheMu.Unlock()
	return ctxText, nil
}

// recallCacheKey canonicalizes one recall request into a cache key. Options
// is a value type; the After/Before pointers are only read here (no aliasing
// writes across goroutines), so the value snapshot is safe.
func recallCacheKey(agentID, query string, opts Options) string {
	var b strings.Builder
	b.WriteString(agentID)
	b.WriteString("\x00")
	b.WriteString(query)
	b.WriteString("\x00")
	fmt.Fprintf(&b, "topk=%d;wiki=%v;minscore=%.4f;", opts.TopK, opts.IncludeWiki, opts.MinScore)
	if opts.After != nil {
		fmt.Fprintf(&b, "after=%s;", opts.After.UTC().Format(time.RFC3339))
	}
	if opts.Before != nil {
		fmt.Fprintf(&b, "before=%s;", opts.Before.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "days=%d", opts.RecentDays)
	return b.String()
}

// assembleContext renders graded hits, annotating each with its source
// namespace when cross-namespace reads are enabled.
func (s *service) assembleContext(graded []verify.Graded) string {
	var b strings.Builder
	for _, g := range graded {
		b.WriteString(renderOneWithSource(g, s.annotate))
	}
	return b.String()
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
	// Double insurance: when both vector search and the vector index's own
	// keyword path come up empty (SQLiteVec.SearchByKeywords is a no-op),
	// fall back to the store's full-text LIKE search.
	if len(hits) == 0 {
		ft, ferr := s.fullTextFallback(ctx, agentID, query, opts.TopK)
		if ferr == nil && len(ft) > 0 {
			hits = ft
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

	// Preload event times in one batch when temporal filtering is active
	// (avoids one GetInterestPoint/GetPage per hit).
	var eventTimes map[string]time.Time
	if after != nil || before != nil {
		eventTimes = s.eventTimes(ctx, agentID, hits)
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
			et := eventTimes[h.ID]
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

// eventTimes resolves EventTime for a batch of hits in two queries (interest
// points first; pages fill in any id not already resolved).
func (s *service) eventTimes(ctx context.Context, agentID string, hits []vec.Hit) map[string]time.Time {
	out := make(map[string]time.Time, len(hits))
	var ipIDs, pgIDs []string
	for _, h := range hits {
		if h.Kind == "interest_point" {
			ipIDs = append(ipIDs, h.ID)
		} else {
			pgIDs = append(pgIDs, h.ID)
		}
	}
	if len(ipIDs) > 0 {
		if ips, err := s.store.GetInterestPointsByIDs(ctx, agentID, ipIDs); err == nil {
			for _, p := range ips {
				out[p.ID] = p.EventTime
			}
		}
	}
	if len(pgIDs) > 0 {
		if pgs, err := s.store.GetPagesByIDs(ctx, agentID, pgIDs); err == nil {
			for _, p := range pgs {
				if _, ok := out[p.ID]; !ok {
					out[p.ID] = p.EventTime
				}
			}
		}
	}
	return out
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

// fullTextFallback runs LIKE keyword search over stored interest points and
// wiki pages (the store's own text columns), mapping results to vector hits
// with their title in Meta. It backs the SQLiteVec deployment where
// SearchByKeywords is a no-op. Archived/superseded entities are filtered out
// here so they never surface in fallback results.
func (s *service) fullTextFallback(ctx context.Context, agentID, query string, topK int) ([]vec.Hit, error) {
	if s.store == nil || strings.TrimSpace(query) == "" || topK <= 0 {
		return nil, nil
	}
	var out []vec.Hit
	if ips, err := s.store.SearchInterestPointsByKeywords(ctx, agentID, query, topK); err == nil {
		for _, p := range ips {
			if p.Status == "archived" {
				continue
			}
			out = append(out, vec.Hit{
				ID:      p.ID,
				AgentID: agentID,
				Kind:    "interest_point",
				Score:   0.5,
				Meta:    map[string]string{"title": p.Name},
			})
		}
	}
	if pgs, err := s.store.SearchPagesByKeywords(ctx, agentID, query, topK); err == nil {
		for _, p := range pgs {
			if p.Status != "" && p.Status != "active" {
				continue
			}
			out = append(out, vec.Hit{
				ID:      p.ID,
				AgentID: agentID,
				Kind:    "wiki_page",
				Score:   0.5,
				Meta:    map[string]string{"title": p.Title},
			})
		}
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// renderOne renders one graded recall line (no source annotation).
func renderOne(g verify.Graded) string {
	return renderOneWithSource(g, false)
}

// renderOneWithSource renders one graded recall line; when annotate is true
// and the hit carries a source agent, it appends `[from: <agent>]`.
func renderOneWithSource(g verify.Graded, annotate bool) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("- [%s] %s [%s]", g.Hit.ID, titleOf(g), g.Hit.Kind))
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
	if annotate && g.Hit.AgentID != "" {
		b.WriteString(fmt.Sprintf(" [from: %s]", g.Hit.AgentID))
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
	var all []vec.Hit
	for _, ns := range s.visible(ctx, agentID) {
		all = append(all, s.searchHits(ctx, ns, query, topK)...)
	}
	if len(all) == 0 {
		return nil, nil
	}
	all = dedupeHits(all)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if len(all) > topK {
		all = all[:topK]
	}
	var out []Result
	ec := newEntityCache()
	for _, h := range all {
		r, err := s.resultFor(ctx, h.AgentID, h, maxBodyLen, ec)
		if err != nil {
			return nil, err
		}
		if r != nil {
			if s.annotate && h.AgentID != "" {
				r.Agent = h.AgentID
			}
			out = append(out, *r)
		}
	}
	return out, nil
}

// searchHits runs the retrieval chain for one namespace: vector search, then
// vector-keyword, then store full-text fallback (first non-empty wins).
func (s *service) searchHits(ctx context.Context, agentID, query string, topK int) []vec.Hit {
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
	if len(hits) == 0 {
		ft, ferr := s.fullTextFallback(ctx, agentID, query, topK)
		if ferr == nil && len(ft) > 0 {
			hits = ft
		}
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

// GetByID returns one entity with full content + edges, or nil when unknown.
// Looks across every visible namespace (the agent's own first), returning the
// first hit. Archived/superseded entities are returned with their status and a
// replacement ref so the consumer can confirm what superseded them (unlike
// Search, which silently substitutes the replacement).
func (s *service) GetByID(ctx context.Context, agentID, id string, maxBodyLen int) (*Result, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	for _, ns := range s.visible(ctx, agentID) {
		r, err := s.getByIDIn(ctx, ns, id, maxBodyLen, newEntityCache())
		if err != nil {
			return nil, err
		}
		if r != nil {
			if s.annotate {
				r.Agent = ns
			}
			return r, nil
		}
	}
	return nil, nil
}

// getByIDIn resolves one entity within a single namespace.
func (s *service) getByIDIn(ctx context.Context, agentID, id string, maxBodyLen int, ec *entityCache) (*Result, error) {
	if p, err := s.store.GetInterestPoint(ctx, agentID, id); err == nil && p != nil {
		// Archived: surface the record itself with its status + replacement.
		if p.Status == "archived" {
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
			s.attachEdges(ctx, agentID, id, r, ec)
			s.attachReplacement(ctx, agentID, id, r)
			return r, nil
		}
		return s.resultFor(ctx, agentID, vec.Hit{ID: id, Kind: "interest_point"}, maxBodyLen, ec)
	}
	pg, err := s.store.GetPage(ctx, agentID, id)
	if err != nil || pg == nil {
		return nil, nil
	}
	// Archived/superseded page: return its own state + replacement.
	if pg.Status != "" && pg.Status != "active" {
		r := &Result{
			Kind:      "wiki_page",
			ID:        pg.ID,
			Title:     pg.Title,
			BodyMD:    truncate(pg.BodyMD, maxBodyLen),
			Status:    pg.Status,
			Claims:    s.pageClaims(ctx, agentID, pg.ID),
			Freshness: store.Freshness{Level: "unknown"},
		}
		s.attachEdges(ctx, agentID, id, r, ec)
		s.attachReplacement(ctx, agentID, id, r)
		return r, nil
	}
	return s.resultFor(ctx, agentID, vec.Hit{ID: id, Kind: "wiki_page"}, maxBodyLen, ec)
}

// resultFor assembles one Result for a hit, attaching edges with far-end
// titles. Archived/superseded entities without a live replacement are
// filtered out; when a replacement exists the hit is silently substituted
// with the successor page (design: silent replacement substitution).
func (s *service) resultFor(ctx context.Context, agentID string, h vec.Hit, maxBodyLen int, ec *entityCache) (*Result, error) {
	if ec == nil {
		ec = newEntityCache()
	}
	if h.Kind == "interest_point" {
		p, err := ec.interestPoint(ctx, agentID, h.ID, s.store)
		if err != nil || p == nil {
			return nil, nil
		}
		if p.Status != "archived" {
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
			s.attachEdges(ctx, agentID, h.ID, r, ec)
			return r, nil
		}
	} else {
		pg, err := ec.page(ctx, agentID, h.ID, s.store)
		if err != nil || pg == nil {
			return nil, nil
		}
		if pg.Status == "" || pg.Status == "active" {
			r := &Result{
				Kind:      "wiki_page",
				ID:        pg.ID,
				Title:     pg.Title,
				BodyMD:    truncate(pg.BodyMD, maxBodyLen),
				Status:    pg.Status,
				Claims:    s.pageClaims(ctx, agentID, pg.ID),
				Freshness: store.Freshness{Level: "unknown"},
			}
			s.attachEdges(ctx, agentID, h.ID, r, ec)
			return r, nil
		}
	}
	// Archived/superseded: silently substitute the live replacement.
	rep, err := s.store.ResolveReplacement(ctx, agentID, h.ID)
	if err != nil || rep == nil {
		return nil, nil
	}
	if rep.Page != nil {
		return s.resultFor(ctx, agentID, vec.Hit{ID: rep.Page.ID, Kind: "wiki_page", Score: h.Score}, maxBodyLen, ec)
	}
	return s.resultFor(ctx, agentID, vec.Hit{ID: rep.InterestPointID, Kind: "interest_point", Score: h.Score}, maxBodyLen, ec)
}

// attachEdges fills Outlinks/Backlinks, resolving the far-end title (page
// Title or interest point Name) for each edge in batch rather than per edge.
func (s *service) attachEdges(ctx context.Context, agentID, id string, r *Result, ec *entityCache) {
	if ec == nil {
		ec = newEntityCache()
	}
	outs, _ := s.store.Outlinks(ctx, agentID, id)
	ins, _ := s.store.Backlinks(ctx, agentID, id)

	// Collect every far-end id and resolve its title in two batch queries.
	farIDs := make([]string, 0, len(outs)+len(ins))
	for _, e := range outs {
		farIDs = append(farIDs, e.TargetID)
	}
	for _, e := range ins {
		farIDs = append(farIDs, e.SourceID)
	}
	titles := s.resolveTitles(ctx, agentID, farIDs, ec)

	for _, e := range outs {
		r.Outlinks = append(r.Outlinks, EdgeRef{ID: e.TargetID, Title: titles[e.TargetID], Kind: e.Kind, Weight: e.Weight})
	}
	for _, e := range ins {
		r.Backlinks = append(r.Backlinks, EdgeRef{ID: e.SourceID, Title: titles[e.SourceID], Kind: e.Kind, Weight: e.Weight})
	}
}

// resolveTitles maps entity ids to their display titles in one batch (interest
// points win over pages on an id collision). Batch results are memoized in the
// entityCache so repeated attachEdges calls (across hits) share one query.
func (s *service) resolveTitles(ctx context.Context, agentID string, ids []string, ec *entityCache) map[string]string {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	var uncached []string
	for _, id := range ids {
		if t, ok := ec.titles[id]; ok {
			out[id] = t
		} else {
			uncached = append(uncached, id)
		}
	}
	if len(uncached) == 0 {
		return out
	}
	if ips, err := s.store.GetInterestPointsByIDs(ctx, agentID, uncached); err == nil {
		for _, p := range ips {
			out[p.ID] = p.Name
			ec.titles[p.ID] = p.Name
		}
	}
	if pgs, err := s.store.GetPagesByIDs(ctx, agentID, uncached); err == nil {
		for _, p := range pgs {
			if _, ok := out[p.ID]; !ok {
				out[p.ID] = p.Title
				ec.titles[p.ID] = p.Title
			}
		}
	}
	return out
}

// attachReplacement resolves and attaches the live successor of an
// archived/superseded entity (best-effort; nil on any failure).
func (s *service) attachReplacement(ctx context.Context, agentID, id string, r *Result) {
	rep, err := s.store.ResolveReplacement(ctx, agentID, id)
	if err != nil || rep == nil {
		return
	}
	if rep.Page != nil {
		r.Replacement = &ReplacementRef{Kind: "wiki_page", ID: rep.Page.ID, Title: rep.Page.Title, Status: rep.Page.Status}
		return
	}
	r.Replacement = &ReplacementRef{Kind: "interest_point", ID: rep.InterestPointID}
	if p, err := s.store.GetInterestPoint(ctx, agentID, rep.InterestPointID); err == nil && p != nil {
		r.Replacement.Title = p.Name
		r.Replacement.Status = p.Status
	}
}

// pageClaims loads a page's claims for result assembly (best-effort; empty on
// any error, since GetPage no longer eager-loads them).
func (s *service) pageClaims(ctx context.Context, agentID, id string) []store.Claim {
	if claims, err := s.store.ListClaims(ctx, agentID, id); err == nil {
		return claims
	}
	return nil
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// Byte-level cuts can split a multi-byte UTF-8 rune (CJK content is
	// 3 bytes per rune), producing invalid UTF-8 in stored excerpts, LLM
	// prompts and JSON output. Back off to the previous rune boundary.
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		_, size := utf8.DecodeLastRuneInString(cut)
		cut = cut[:len(cut)-size]
	}
	return cut + "..."
}

var _ RecallService = (*service)(nil)
