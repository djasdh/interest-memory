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
