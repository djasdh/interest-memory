package recall

import (
	"context"
	"strings"
	"testing"

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

type fakeGrader struct{}

func (fakeGrader) GradeForRecall(_ context.Context, _ string, hits []vec.Hit) ([]verify.Graded, error) {
	out := make([]verify.Graded, 0, len(hits))
	for _, h := range hits {
		out = append(out, verify.Graded{
			Hit:        h,
			Title:      "T-" + h.ID,
			Confidence: 0.8,
			Status:     "supported",
			FreshLevel: "fresh",
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
	s := New(fakeEmbedder{}, fv, &fakeStore{}, fakeGrader{})
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
	s := New(fakeEmbedder{}, fv, &fakeStore{}, fakeGrader{})
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
	s := New(fakeEmbedder{}, fv, &fakeStore{}, fakeGrader{})
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
	s := New(fakeEmbedder{}, &fakeVec{}, &fakeStore{}, fakeGrader{})
	out, err := s.Recall(context.Background(), "agent-a", "  ", Options{TopK: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("empty query should return empty context, got %q", out)
	}
}

func TestRecallEmptyHits(t *testing.T) {
	s := New(fakeEmbedder{}, &fakeVec{}, &fakeStore{}, fakeGrader{})
	out, err := s.Recall(context.Background(), "agent-a", "nothing matches", Options{TopK: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("no hits should return empty context, got %q", out)
	}
}
