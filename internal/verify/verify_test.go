package verify

import (
	"context"
	"testing"

	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
)

type fakeStore struct {
	ip   *store.InterestPoint
	page *store.Page
}

func (f *fakeStore) GetInterestPoint(_ context.Context, _, _ string) (*store.InterestPoint, error) {
	return f.ip, nil
}
func (f *fakeStore) GetPage(_ context.Context, _, _ string) (*store.Page, error) {
	return f.page, nil
}
func (f *fakeStore) ListClaims(_ context.Context, _, _ string) ([]store.Claim, error) {
	if f.page == nil {
		return nil, nil
	}
	return f.page.Claims, nil
}
func (f *fakeStore) ResolveReplacement(context.Context, string, string) (*store.Replacement, error) {
	return nil, nil
}

func TestGradeForRecallInterestPoint(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip1", AgentID: "a", Name: "Alpha",
		Reliability: store.Reliability{Confidence: 0.7, Status: "supported"},
		Freshness:   store.Freshness{Level: "aging"}}
	st := &fakeStore{ip: ip}
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{
		{ID: "ip1", AgentID: "a", Kind: "interest_point", Score: 0.8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("graded = %d, want 1", len(got))
	}
	g := got[0]
	if g.Title != "Alpha" || g.Confidence != 0.7 || g.Status != "supported" || g.FreshLevel != "aging" {
		t.Errorf("graded = %+v", g)
	}
	if g.Note == "" {
		t.Error("expected self-check note")
	}
}

func TestGradeForRecallArchivedFiltered(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip1", AgentID: "a", Name: "Alpha", Status: "archived",
		Reliability: store.Reliability{Confidence: 0.7, Status: "supported"},
		Freshness:   store.Freshness{Level: "aging"}}
	st := &fakeStore{ip: ip}
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{
		{ID: "ip1", AgentID: "a", Kind: "interest_point", Score: 0.8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("graded = %d, want 0 (archived filtered)", len(got))
	}
}

func TestGradeForRecallSupersededPageFiltered(t *testing.T) {
	st := &fakeStore{page: &store.Page{ID: "pg1", Title: "Page", Status: "superseded", Claims: []store.Claim{
		{Text: "a", Confidence: 0.9, Status: "supported"},
	}}}
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "pg1", Kind: "wiki_page", Score: 0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("graded = %d, want 0 (superseded page filtered)", len(got))
	}
}

// TestGradeForRecallGhostInterestPointFiltered checks that a vector hit whose
// entity no longer exists in the store (stale index entry / concurrent
// deletion) is skipped instead of being injected as an empty-title "unknown"
// graded hit.
func TestGradeForRecallGhostInterestPointFiltered(t *testing.T) {
	st := &fakeStore{} // no ip stored
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{
		{ID: "ghost-ip", AgentID: "a", Kind: "interest_point", Score: 0.9},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("graded = %d, want 0 (ghost interest point filtered); got %+v", len(got), got)
	}
}

// TestGradeForRecallGhostPageFiltered is the wiki-page twin: a stale vector
// hit for a page that is not in the store must not become an empty "unknown"
// row either.
func TestGradeForRecallGhostPageFiltered(t *testing.T) {
	st := &fakeStore{} // no page stored
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{
		{ID: "ghost-page", AgentID: "a", Kind: "wiki_page", Score: 0.7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("graded = %d, want 0 (ghost page filtered); got %+v", len(got), got)
	}
}

// fakeSubStore extends fakeStore with a configurable replacement so grading
// can exercise silent substitution of superseded entities.
type fakeSubStore struct {
	ip       *store.InterestPoint
	page     *store.Page
	replPage *store.Page
	replIP   *store.InterestPoint
}

func (f *fakeSubStore) GetInterestPoint(_ context.Context, _, id string) (*store.InterestPoint, error) {
	if f.replIP != nil && id == f.replIP.ID {
		return f.replIP, nil
	}
	return f.ip, nil
}
func (f *fakeSubStore) GetPage(_ context.Context, _, id string) (*store.Page, error) {
	if f.replPage != nil && id == f.replPage.ID {
		return f.replPage, nil
	}
	return f.page, nil
}
func (f *fakeSubStore) ListClaims(_ context.Context, _, id string) ([]store.Claim, error) {
	pg := f.page
	if f.replPage != nil && id == f.replPage.ID {
		pg = f.replPage
	}
	if pg == nil {
		return nil, nil
	}
	return pg.Claims, nil
}
func (f *fakeSubStore) ResolveReplacement(context.Context, string, string) (*store.Replacement, error) {
	if f.replPage != nil {
		return &store.Replacement{InterestPointID: f.replIP.ID, Page: f.replPage}, nil
	}
	if f.replIP != nil {
		return &store.Replacement{InterestPointID: f.replIP.ID}, nil
	}
	return nil, nil
}

func TestGradeForRecallSubstitutesSupersededPage(t *testing.T) {
	st := &fakeSubStore{
		page: &store.Page{ID: "pg-old", Title: "旧页", Status: "superseded", Claims: []store.Claim{
			{Text: "old", Confidence: 0.9, Status: "supported"},
		}},
		replPage: &store.Page{ID: "pg-new", Title: "新页", Status: "active", Claims: []store.Claim{
			{Text: "new", Confidence: 0.95, Status: "supported", Freshness: store.Freshness{Level: "fresh"}},
		}},
		replIP: &store.InterestPoint{ID: "ip-new", Name: "新点"},
	}
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "pg-old", Kind: "wiki_page", Score: 0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("graded = %d, want 1 (substituted)", len(got))
	}
	g := got[0]
	if g.Hit.ID != "pg-new" || g.Hit.Kind != "wiki_page" {
		t.Errorf("substituted hit = %+v, want pg-new", g.Hit)
	}
	if g.Title != "新页" {
		t.Errorf("title = %q, want 新页", g.Title)
	}
}

func TestGradeForRecallSubstitutesArchivedInterestPoint(t *testing.T) {
	st := &fakeSubStore{
		ip:     &store.InterestPoint{ID: "ip-old", Name: "旧点", Status: "archived"},
		replIP: &store.InterestPoint{ID: "ip-new", Name: "新点"},
		replPage: &store.Page{ID: "pg-new", Title: "新页", Status: "active", Claims: []store.Claim{
			{Text: "n", Confidence: 0.9, Status: "supported"},
		}},
	}
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "ip-old", Kind: "interest_point", Score: 0.8}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hit.ID != "pg-new" {
		t.Errorf("graded = %+v, want substituted pg-new", got)
	}
}

func TestGradeForRecallPage(t *testing.T) {
	st := &fakeStore{page: &store.Page{ID: "pg1", Title: "Page", Claims: []store.Claim{
		{Text: "a", Confidence: 0.4, Status: "contested", Freshness: store.Freshness{Level: "stale"}},
		{Text: "b", Confidence: 0.9, Status: "supported", Freshness: store.Freshness{Level: "fresh"}},
	}}}
	v := New(st)
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "pg1", Kind: "wiki_page", Score: 0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Title != "Page" || got[0].Status != "supported" || got[0].FreshLevel != "fresh" {
		t.Errorf("page graded = %+v", got[0])
	}
}
