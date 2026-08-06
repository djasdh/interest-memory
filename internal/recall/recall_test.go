package recall

import (
	"context"
	"strings"
	"testing"
	"time"

	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/verify"
)

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}

type fakeVec struct {
	hits []vec.Hit
}

func (f *fakeVec) Search(_ context.Context, _ string, _ []float32, _ int) ([]vec.Hit, error) {
	return f.hits, nil
}
func (f *fakeVec) SearchByKeywords(_ context.Context, _ string, _ string, _ int) ([]vec.Hit, error) {
	return f.hits, nil
}

type fakeStore struct {
	ips  map[string]*store.InterestPoint
	pgs  map[string]*store.Page
	outs map[string][]store.Edge
	ins  map[string][]store.Edge
}

func (f *fakeStore) GetInterestPoint(_ context.Context, _, id string) (*store.InterestPoint, error) {
	if p, ok := f.ips[id]; ok {
		return p, nil
	}
	return nil, nil
}
func (f *fakeStore) GetPage(_ context.Context, _, id string) (*store.Page, error) {
	if p, ok := f.pgs[id]; ok {
		return p, nil
	}
	return nil, nil
}
func (f *fakeStore) Outlinks(_ context.Context, _, id string) ([]store.Edge, error) {
	return f.outs[id], nil
}
func (f *fakeStore) Backlinks(_ context.Context, _, id string) ([]store.Edge, error) {
	return f.ins[id], nil
}
func (f *fakeStore) SearchInterestPointsByKeywords(_ context.Context, _, query string, _ int) ([]store.InterestPoint, error) {
	var out []store.InterestPoint
	for _, p := range f.ips {
		if strings.Contains(p.Name, query) || strings.Contains(p.Summary, query) {
			out = append(out, *p)
		}
	}
	return out, nil
}
func (f *fakeStore) SearchPagesByKeywords(_ context.Context, _, query string, _ int) ([]store.Page, error) {
	var out []store.Page
	for _, p := range f.pgs {
		if strings.Contains(p.Title, query) || strings.Contains(p.BodyMD, query) {
			out = append(out, *p)
		}
	}
	return out, nil
}
func (f *fakeStore) ResolveReplacement(_ context.Context, _, id string) (*store.Replacement, error) {
	ipID := id
	if _, ok := f.pgs[id]; ok {
		ipID = ""
		for _, e := range f.ins[id] {
			if e.Kind == store.EdgeHasPage {
				ipID = e.SourceID
				break
			}
		}
	}
	cur := ipID
	for cur != "" {
		var next string
		for _, e := range f.outs[cur] {
			if e.Kind == store.EdgeSequel {
				next = e.TargetID
				break
			}
		}
		if next == "" {
			return nil, nil
		}
		ip, ok := f.ips[next]
		if !ok || ip == nil {
			return nil, nil
		}
		if ip.Status == "archived" {
			cur = next
			continue
		}
		rep := &store.Replacement{InterestPointID: next}
		for _, oe := range f.outs[next] {
			if oe.Kind == store.EdgeHasPage {
				if pg, ok := f.pgs[oe.TargetID]; ok && pg != nil {
					rep.Page = pg
					break
				}
			}
		}
		return rep, nil
	}
	return nil, nil
}

type fakeGrader struct{ evt time.Time }

func (f *fakeGrader) GradeForRecall(_ context.Context, _ string, hits []vec.Hit) ([]verify.Graded, error) {
	out := make([]verify.Graded, 0, len(hits))
	for _, h := range hits {
		out = append(out, verify.Graded{
			Hit:        h,
			Title:      "T-" + h.ID,
			Confidence: 0.8,
			Status:     "supported",
			FreshLevel: "fresh",
			EventTime:  f.evt,
			Note:       "may be outdated or inaccurate — please verify on your own",
		})
	}
	return out, nil
}

func TestRecallAssemblesMemoryContext(t *testing.T) {
	fv := &fakeVec{hits: []vec.Hit{
		{ID: "ip1", AgentID: "a", Kind: "interest_point", Score: 0.9},
		{ID: "pg1", AgentID: "a", Kind: "wiki_page", Score: 0.7},
	}}
	s := New(fakeEmbedder{}, fv, &fakeStore{}, &fakeGrader{})
	out, err := s.Recall(context.Background(), "agent-a", "go concurrency", Options{TopK: 8, IncludeWiki: true, MinScore: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<memory-context>") || strings.Contains(out, "</memory-context>") {
		t.Fatalf("should return bare text (no fence): %q", out)
	}
	if !strings.Contains(out, "[ip1]") || !strings.Contains(out, "[pg1]") {
		t.Errorf("missing hit ids: %s", out)
	}
	if !strings.Contains(out, "T-ip1") || !strings.Contains(out, "T-pg1") {
		t.Errorf("missing hits: %s", out)
	}
	if !strings.Contains(out, "please verify") {
		t.Errorf("missing self-check note: %s", out)
	}
}

func TestRecallExcludesWikiWhenDisabled(t *testing.T) {
	fv := &fakeVec{hits: []vec.Hit{
		{ID: "ip1", Kind: "interest_point", Score: 0.9},
		{ID: "pg1", Kind: "wiki_page", Score: 0.9},
	}}
	s := New(fakeEmbedder{}, fv, &fakeStore{}, &fakeGrader{})
	out, err := s.Recall(context.Background(), "agent-a", "q", Options{TopK: 8, IncludeWiki: false})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "T-pg1") {
		t.Errorf("wiki page should be excluded: %s", out)
	}
	if !strings.Contains(out, "T-ip1") {
		t.Errorf("interest point missing: %s", out)
	}
}

func TestRecallFiltersByMinScore(t *testing.T) {
	fv := &fakeVec{hits: []vec.Hit{
		{ID: "ip1", Kind: "interest_point", Score: 0.9},
		{ID: "ip2", Kind: "interest_point", Score: 0.1},
	}}
	s := New(fakeEmbedder{}, fv, &fakeStore{}, &fakeGrader{})
	out, err := s.Recall(context.Background(), "agent-a", "q", Options{TopK: 8, IncludeWiki: true, MinScore: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "T-ip2") {
		t.Errorf("low-score hit not filtered: %s", out)
	}
	if !strings.Contains(out, "T-ip1") {
		t.Errorf("high-score hit missing: %s", out)
	}
}

func TestRecallEmptyQuery(t *testing.T) {
	s := New(fakeEmbedder{}, &fakeVec{}, &fakeStore{}, &fakeGrader{})
	out, err := s.Recall(context.Background(), "agent-a", "  ", Options{TopK: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("empty query should return empty context, got %q", out)
	}
}

func TestRecallFiltersByAfter(t *testing.T) {
	now := time.Now().UTC()
	after := now.Add(-24 * time.Hour)
	old := &store.InterestPoint{ID: "ip-old", AgentID: "a", Name: "old", EventTime: now.Add(-48 * time.Hour)}
	newIP := &store.InterestPoint{ID: "ip-new", AgentID: "a", Name: "new", EventTime: now.Add(-1 * time.Hour)}
	st := &fakeStore{
		ips: map[string]*store.InterestPoint{"ip-old": old, "ip-new": newIP},
	}
	fv := &fakeVec{hits: []vec.Hit{
		{ID: "ip-old", Kind: "interest_point", Score: 0.9},
		{ID: "ip-new", Kind: "interest_point", Score: 0.8},
	}}
	s := New(fakeEmbedder{}, fv, st, &fakeGrader{})
	out, err := s.Recall(context.Background(), "a", "q", Options{TopK: 8, After: &after})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "ip-old") || !strings.Contains(out, "ip-new") {
		t.Errorf("after-filter output = %q", out)
	}
}

func TestRecallFiltersByRecentDays(t *testing.T) {
	old := &store.InterestPoint{ID: "ip-old", AgentID: "a", Name: "old", EventTime: time.Now().Add(-10 * 24 * time.Hour)}
	newIP := &store.InterestPoint{ID: "ip-new", AgentID: "a", Name: "new", EventTime: time.Now().Add(-1 * time.Hour)}
	st := &fakeStore{ips: map[string]*store.InterestPoint{"ip-old": old, "ip-new": newIP}}
	fv := &fakeVec{hits: []vec.Hit{
		{ID: "ip-old", Kind: "interest_point", Score: 0.9},
		{ID: "ip-new", Kind: "interest_point", Score: 0.8},
	}}
	s := New(fakeEmbedder{}, fv, st, &fakeGrader{})
	out, err := s.Recall(context.Background(), "a", "q", Options{TopK: 8, RecentDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "ip-old") || !strings.Contains(out, "ip-new") {
		t.Errorf("recent-days output = %q", out)
	}
}

func TestRecallRendersEventTime(t *testing.T) {
	evt := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ip := &store.InterestPoint{ID: "ip-1", AgentID: "a", Name: "Alpha", EventTime: evt}
	st := &fakeStore{ips: map[string]*store.InterestPoint{"ip-1": ip}}
	fv := &fakeVec{hits: []vec.Hit{{ID: "ip-1", Kind: "interest_point", Score: 0.9}}}
	s := New(fakeEmbedder{}, fv, st, &fakeGrader{evt: evt})
	out, err := s.Recall(context.Background(), "a", "q", Options{TopK: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(at 2026-08-01)") {
		t.Errorf("output missing event timestamp: %q", out)
	}
}

func TestRecallEmptyHits(t *testing.T) {
	s := New(fakeEmbedder{}, &fakeVec{}, &fakeStore{}, &fakeGrader{})
	out, err := s.Recall(context.Background(), "agent-a", "nothing matches", Options{TopK: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("no hits should return empty context, got %q", out)
	}
}

func TestSearchReturnsMixedResultsWithEdges(t *testing.T) {
	ip := &store.InterestPoint{
		ID: "ip-1", AgentID: "a", Name: "PostgreSQL", Summary: "默认数据库，JSONB 好用",
		Status: "active", TurnRange: [2]int{1, 2},
		Reliability: store.Reliability{Confidence: 0.9, Status: "supported",
			Evidence: []store.Evidence{{Kind: "web", URL: "https://x", Query: "pg jsonb"}}},
		Freshness: store.Freshness{Level: "fresh"},
	}
	pg := &store.Page{
		ID: "postgresql-page", AgentID: "a", Title: "PostgreSQL 决策", Status: "active",
		BodyMD: "我们用 PostgreSQL 作为默认数据库。",
		Claims: []store.Claim{{ID: "c1", Text: "JSONB 支持", Status: "supported", Confidence: 0.9}},
	}
	st := &fakeStore{
		ips: map[string]*store.InterestPoint{"ip-1": ip},
		pgs: map[string]*store.Page{"postgresql-page": pg},
		outs: map[string][]store.Edge{
			"ip-1":            {{SourceID: "ip-1", TargetID: "postgresql-page", Kind: store.EdgeHasPage, Weight: 1}},
			"postgresql-page": {{SourceID: "postgresql-page", TargetID: "related-page", Kind: store.EdgeRelated, Weight: 0.9}},
		},
		ins: map[string][]store.Edge{
			"postgresql-page": {{SourceID: "ip-1", TargetID: "postgresql-page", Kind: store.EdgeHasPage, Weight: 1}},
		},
	}
	// Target pages for edge-title resolution.
	st.pgs["related-page"] = &store.Page{ID: "related-page", Title: "相关页"}
	fv := &fakeVec{hits: []vec.Hit{
		{ID: "ip-1", Kind: "interest_point", Score: 0.9},
		{ID: "postgresql-page", Kind: "wiki_page", Score: 0.85},
	}}
	s := New(fakeEmbedder{}, fv, st, &fakeGrader{})
	results, err := s.Search(context.Background(), "a", "PostgreSQL", 5, 4000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	var rp, ri *Result
	for i := range results {
		switch results[i].Kind {
		case "wiki_page":
			rp = &results[i]
		case "interest_point":
			ri = &results[i]
		}
	}
	if rp == nil || ri == nil {
		t.Fatalf("expected both kinds, got %+v", results)
	}
	if ri.Title != "PostgreSQL" || len(ri.Evidence) != 1 || ri.Evidence[0].URL != "https://x" {
		t.Errorf("interest result = %+v", ri)
	}
	if len(ri.Outlinks) != 1 || ri.Outlinks[0].ID != "postgresql-page" || ri.Outlinks[0].Kind != store.EdgeHasPage {
		t.Errorf("interest outlinks = %+v", ri.Outlinks)
	}
	if rp.BodyMD != "我们用 PostgreSQL 作为默认数据库。" {
		t.Errorf("page body = %q", rp.BodyMD)
	}
	if len(rp.Claims) != 1 || rp.Claims[0].Text != "JSONB 支持" {
		t.Errorf("page claims = %+v", rp.Claims)
	}
	if len(rp.Backlinks) != 1 || rp.Backlinks[0].ID != "ip-1" {
		t.Errorf("page backlinks = %+v", rp.Backlinks)
	}
	if rp.Outlinks[0].Title != "相关页" {
		t.Errorf("outlink should carry target title, got %+v", rp.Outlinks[0])
	}
}

func TestSearchTruncatesBody(t *testing.T) {
	longBody := strings.Repeat("x", 5000)
	pg := &store.Page{ID: "pg", AgentID: "a", Title: "T", Status: "active", BodyMD: longBody}
	st := &fakeStore{
		pgs:  map[string]*store.Page{"pg": pg},
		outs: map[string][]store.Edge{},
		ins:  map[string][]store.Edge{},
	}
	s := New(fakeEmbedder{}, &fakeVec{hits: []vec.Hit{{ID: "pg", Kind: "wiki_page", Score: 0.9}}}, st, &fakeGrader{})
	results, err := s.Search(context.Background(), "a", "q", 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0].BodyMD) > 103 || !strings.HasSuffix(results[0].BodyMD, "...") {
		t.Errorf("body not truncated to maxBodyLen: %d", len(results[0].BodyMD))
	}
}

func TestSearchNoHits(t *testing.T) {
	s := New(fakeEmbedder{}, &fakeVec{}, &fakeStore{}, &fakeGrader{})
	results, err := s.Search(context.Background(), "a", "nothing", 5, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestGetByIDResolvesPageAndInterestPoint(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip-1", AgentID: "a", Name: "点", Status: "active", Summary: "s",
		Reliability: store.Reliability{Evidence: []store.Evidence{{Kind: "session"}}}}
	st := &fakeStore{
		ips:  map[string]*store.InterestPoint{"ip-1": ip},
		outs: map[string][]store.Edge{},
		ins:  map[string][]store.Edge{},
	}
	s := New(fakeEmbedder{}, &fakeVec{}, st, &fakeGrader{})
	r, err := s.GetByID(context.Background(), "a", "ip-1", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Kind != "interest_point" || r.Title != "点" {
		t.Errorf("by id = %+v", r)
	}
	missing, err := s.GetByID(context.Background(), "a", "nope", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Errorf("missing id should return nil, got %+v", missing)
	}
}

func TestSearchSilentlySubstitutesSupersededPage(t *testing.T) {
	oldPg := &store.Page{ID: "pg-old", AgentID: "a", Title: "旧页", Status: "superseded", BodyMD: "old content"}
	newPg := &store.Page{ID: "pg-new", AgentID: "a", Title: "新页", Status: "active", BodyMD: "new content"}
	oldIP := &store.InterestPoint{ID: "ip-old", AgentID: "a", Name: "旧点", Status: "archived"}
	newIP := &store.InterestPoint{ID: "ip-new", AgentID: "a", Name: "新点", Status: "active"}
	st := &fakeStore{
		ips: map[string]*store.InterestPoint{"ip-old": oldIP, "ip-new": newIP},
		pgs: map[string]*store.Page{"pg-old": oldPg, "pg-new": newPg},
		outs: map[string][]store.Edge{
			"ip-old": {
				{SourceID: "ip-old", TargetID: "pg-old", Kind: store.EdgeHasPage, Weight: 1},
				{SourceID: "ip-old", TargetID: "ip-new", Kind: store.EdgeSequel, Weight: 1},
			},
			"ip-new": {{SourceID: "ip-new", TargetID: "pg-new", Kind: store.EdgeHasPage, Weight: 1}},
			"pg-old": {},
			"pg-new": {},
		},
		ins: map[string][]store.Edge{
			"pg-old": {{SourceID: "ip-old", TargetID: "pg-old", Kind: store.EdgeHasPage, Weight: 1}},
			"pg-new": {{SourceID: "ip-new", TargetID: "pg-new", Kind: store.EdgeHasPage, Weight: 1}},
		},
	}
	s := New(fakeEmbedder{}, &fakeVec{hits: []vec.Hit{{ID: "pg-old", Kind: "wiki_page", Score: 0.9}}}, st, &fakeGrader{})
	results, err := s.Search(context.Background(), "a", "q", 5, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (substituted)", len(results))
	}
	if results[0].ID != "pg-new" || results[0].Title != "新页" || results[0].BodyMD != "new content" {
		t.Errorf("search did not substitute superseded page: %+v", results[0])
	}
}

func TestGetByIDArchivedReturnsStatusAndReplacement(t *testing.T) {
	oldPg := &store.Page{ID: "pg-old", AgentID: "a", Title: "旧页", Status: "superseded", BodyMD: "old"}
	newPg := &store.Page{ID: "pg-new", AgentID: "a", Title: "新页", Status: "active", BodyMD: "new"}
	oldIP := &store.InterestPoint{ID: "ip-old", AgentID: "a", Name: "旧点", Status: "archived"}
	newIP := &store.InterestPoint{ID: "ip-new", AgentID: "a", Name: "新点", Status: "active"}
	st := &fakeStore{
		ips: map[string]*store.InterestPoint{"ip-old": oldIP, "ip-new": newIP},
		pgs: map[string]*store.Page{"pg-old": oldPg, "pg-new": newPg},
		outs: map[string][]store.Edge{
			"ip-old": {
				{SourceID: "ip-old", TargetID: "pg-old", Kind: store.EdgeHasPage, Weight: 1},
				{SourceID: "ip-old", TargetID: "ip-new", Kind: store.EdgeSequel, Weight: 1},
			},
			"ip-new": {{SourceID: "ip-new", TargetID: "pg-new", Kind: store.EdgeHasPage, Weight: 1}},
			"pg-old": {},
			"pg-new": {},
		},
		ins: map[string][]store.Edge{
			"pg-old": {{SourceID: "ip-old", TargetID: "pg-old", Kind: store.EdgeHasPage, Weight: 1}},
			"pg-new": {{SourceID: "ip-new", TargetID: "pg-new", Kind: store.EdgeHasPage, Weight: 1}},
		},
	}
	s := New(fakeEmbedder{}, &fakeVec{}, st, &fakeGrader{})

	r, err := s.GetByID(context.Background(), "a", "pg-old", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("GetByID returned nil for superseded page")
	}
	if r.Status != "superseded" {
		t.Errorf("status = %q, want superseded", r.Status)
	}
	if r.Replacement == nil || r.Replacement.ID != "pg-new" || r.Replacement.Kind != "wiki_page" {
		t.Errorf("replacement = %+v, want pg-new", r.Replacement)
	}
}

func TestRenderOneIncludesID(t *testing.T) {
	g := verify.Graded{
		Hit:        vec.Hit{ID: "ip-abc123", AgentID: "a", Kind: "interest_point", Score: 0.8},
		Title:      "Go 并发",
		Confidence: 0.9,
		Status:     "supported",
		FreshLevel: "fresh",
		Note:       "note",
	}
	out := renderOne(g)
	if !strings.Contains(out, "- [ip-abc123] Go 并发 [interest_point]") {
		t.Errorf("renderOne missing id prefix: %q", out)
	}
	pg := verify.Graded{
		Hit:   vec.Hit{ID: "pg-xyz", AgentID: "a", Kind: "wiki_page", Score: 0.7},
		Title: "PostgreSQL",
	}
	out = renderOne(pg)
	if !strings.Contains(out, "- [pg-xyz] PostgreSQL [wiki_page]") {
		t.Errorf("renderOne missing wiki page id prefix: %q", out)
	}
}

func TestRecallFullTextFallback(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip-ft", AgentID: "a", Name: "Go 并发模型", Summary: "goroutine 与 channel", Status: "active"}
	pg := &store.Page{ID: "pg-ft", AgentID: "a", Title: "并发编程", BodyMD: "goroutine channel 用法", Status: "active"}
	st := &fakeStore{
		ips: map[string]*store.InterestPoint{"ip-ft": ip},
		pgs: map[string]*store.Page{"pg-ft": pg},
	}
	// vec returns nothing; store full-text fallback supplies the hits.
	s := New(fakeEmbedder{}, &fakeVec{}, st, &fakeGrader{})
	out, err := s.Recall(context.Background(), "a", "goroutine", Options{TopK: 8, IncludeWiki: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[ip-ft]") || !strings.Contains(out, "[interest_point]") {
		t.Errorf("recall full-text fallback missing interest point: %q", out)
	}
	if !strings.Contains(out, "[pg-ft]") || !strings.Contains(out, "[wiki_page]") {
		t.Errorf("recall full-text fallback missing page: %q", out)
	}
}

func TestRecallFullTextFallbackRespectsIncludeWiki(t *testing.T) {
	pg := &store.Page{ID: "pg-ft", AgentID: "a", Title: "并发编程", BodyMD: "goroutine 用法", Status: "active"}
	st := &fakeStore{pgs: map[string]*store.Page{"pg-ft": pg}}
	s := New(fakeEmbedder{}, &fakeVec{}, st, &fakeGrader{})
	out, err := s.Recall(context.Background(), "a", "goroutine", Options{TopK: 8, IncludeWiki: false})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "pg-ft") {
		t.Errorf("full-text wiki page should be excluded when IncludeWiki=false: %q", out)
	}
}

func TestSearchFullTextFallback(t *testing.T) {
	pg := &store.Page{ID: "pg-ft", AgentID: "a", Title: "并发编程", BodyMD: "goroutine channel 用法", Status: "active"}
	st := &fakeStore{pgs: map[string]*store.Page{"pg-ft": pg}}
	s := New(fakeEmbedder{}, &fakeVec{}, st, &fakeGrader{})
	results, err := s.Search(context.Background(), "a", "goroutine", 5, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 from full-text fallback", len(results))
	}
	if results[0].ID != "pg-ft" || results[0].Title != "并发编程" {
		t.Errorf("search full-text fallback = %+v", results[0])
	}
}

func TestFullTextFallbackCarriesTitleMeta(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip-ft", AgentID: "a", Name: "Go 并发模型", Summary: "goroutine", Status: "active"}
	pg := &store.Page{ID: "pg-ft", AgentID: "a", Title: "并发编程", BodyMD: "goroutine", Status: "active"}
	st := &fakeStore{
		ips: map[string]*store.InterestPoint{"ip-ft": ip},
		pgs: map[string]*store.Page{"pg-ft": pg},
	}
	s := New(fakeEmbedder{}, &fakeVec{}, st, &fakeGrader{})
	hits, err := s.(*service).fullTextFallback(context.Background(), "a", "goroutine", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	titles := map[string]string{}
	for _, h := range hits {
		titles[h.ID] = h.Meta["title"]
	}
	if titles["ip-ft"] != "Go 并发模型" || titles["pg-ft"] != "并发编程" {
		t.Errorf("fallback hit titles = %v, want ip-ft→Go 并发模型 / pg-ft→并发编程", titles)
	}
}
