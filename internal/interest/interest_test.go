package interest

import (
	"context"
	"testing"

	"interest-memory/internal/config"
	"interest-memory/internal/fork"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/verify"
)

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}

type fakeVec struct {
	hits    []vec.Hit
	deleted []string
}

func (f *fakeVec) Search(_ context.Context, _ string, _ []float32, _ int) ([]vec.Hit, error) {
	return f.hits, nil
}
func (f *fakeVec) Upsert(_ context.Context, _ vec.Entry) error { return nil }
func (f *fakeVec) Delete(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeStore struct {
	ip     map[string]*store.InterestPoint
	edges  []store.Edge
	upsert *store.InterestPoint
}

func newFakeStore() *fakeStore { return &fakeStore{ip: map[string]*store.InterestPoint{}} }

func (f *fakeStore) GetInterestPoint(_ context.Context, _, id string) (*store.InterestPoint, error) {
	if p, ok := f.ip[id]; ok {
		return p, nil
	}
	return nil, nil
}
func (f *fakeStore) UpsertInterestPoint(_ context.Context, p store.InterestPoint) error {
	f.upsert = &p
	f.ip[p.ID] = &p
	return nil
}
func (f *fakeStore) AddEdgePair(_ context.Context, _ string, e store.Edge) error {
	f.edges = append(f.edges, e)
	return nil
}

func vv(topic string, conf float64) verify.Verified {
	return verify.Verified{
		Candidate:   fork.Candidate{Topic: topic, Reason: "reason for " + topic, Confidence: conf, Tags: []string{"t1"}},
		Reliability: store.Reliability{Confidence: conf, Status: "supported"},
		Freshness:   store.Freshness{Level: "fresh"},
	}
}

func TestCleanCreatesNew(t *testing.T) {
	st := newFakeStore()
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{vv("brand new topic", 0.9)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("created = %d, want 1", len(out))
	}
	if out[0].SeenCount != 1 || out[0].Name != "brand new topic" {
		t.Errorf("created point = %+v", out[0])
	}
}

func TestCleanStoresTurnRange(t *testing.T) {
	st := newFakeStore()
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})
	v := vv("带轮次的主题", 0.8)
	v.Candidate.TurnRange = [2]int{2, 7}
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{v})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("created = %d, want 1", len(out))
	}
	if out[0].TurnRange != [2]int{2, 7} {
		t.Errorf("turn_range = %v, want [2 7]", out[0].TurnRange)
	}
}

func TestCleanMergesHighSimilarity(t *testing.T) {
	existing := &store.InterestPoint{ID: "existing-id", AgentID: "agent-a", Name: "old topic",
		Importance: 0.5, SeenCount: 2, Keywords: []string{"old"}}
	st := newFakeStore()
	st.ip["existing-id"] = existing
	fv := &fakeVec{hits: []vec.Hit{{ID: "existing-id", AgentID: "agent-a", Kind: "interest_point", Score: 0.95}}}
	c := New(fakeEmbedder{}, fv, st, config.ForkConfig{})
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{vv("old topic updated", 0.9)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("merged = %d, want 1", len(out))
	}
	p := st.ip["existing-id"]
	if p == nil {
		t.Fatal("existing point missing")
	}
	if p.SeenCount != 3 {
		t.Errorf("seen_count = %d, want 3", p.SeenCount)
	}
	if p.Importance <= 0.5 {
		t.Errorf("importance not boosted: %f", p.Importance)
	}
	if !containsString(p.Keywords, "t1") {
		t.Errorf("keywords not merged: %v", p.Keywords)
	}
	if len(out) != 1 || out[0].ID != "existing-id" {
		t.Errorf("merged output should reference existing id")
	}
}

func TestCleanRelatesMediumSimilarity(t *testing.T) {
	st := newFakeStore()
	fv := &fakeVec{hits: []vec.Hit{{ID: "existing-id", AgentID: "agent-a", Kind: "interest_point", Score: 0.6}}}
	c := New(fakeEmbedder{}, fv, st, config.ForkConfig{})
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{vv("related topic", 0.8)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("created = %d, want 1", len(out))
	}
	if len(st.edges) != 1 {
		t.Fatalf("edges = %d, want 1 related edge", len(st.edges))
	}
	if st.edges[0].Kind != store.EdgeRelated || st.edges[0].TargetID != "existing-id" {
		t.Errorf("edge = %+v", st.edges[0])
	}
}

func TestCleanCreatesOnLowSimilarity(t *testing.T) {
	st := newFakeStore()
	fv := &fakeVec{hits: []vec.Hit{{ID: "existing-id", AgentID: "agent-a", Kind: "interest_point", Score: 0.1}}}
	c := New(fakeEmbedder{}, fv, st, config.ForkConfig{})
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{vv("totally different", 0.7)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("created = %d, want 1", len(out))
	}
	if len(st.edges) != 0 {
		t.Errorf("edges = %d, want 0", len(st.edges))
	}
}

// vvRel builds a verified candidate with an explicit relation.
func vvRel(topic string, rel verify.Relation, toID, reason string) verify.Verified {
	v := vv(topic, 0.9)
	v.Relation = rel
	v.RelationToID = toID
	v.RelationReason = reason
	return v
}

func TestCleanRelationDeleteArchivesOld(t *testing.T) {
	old := &store.InterestPoint{ID: "old1", AgentID: "agent-a", Name: "旧观点", Status: "active"}
	st := newFakeStore()
	st.ip["old1"] = old
	fv := &fakeVec{}
	c := New(fakeEmbedder{}, fv, st, config.ForkConfig{})
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{
		vvRel("新观点", verify.RelationDelete, "old1", "用户推翻旧观点"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("created = %d, want 0 (delete creates nothing)", len(out))
	}
	if old.Status != "archived" {
		t.Errorf("old status = %q, want archived", old.Status)
	}
	if len(fv.deleted) != 1 || fv.deleted[0] != "old1" {
		t.Errorf("vec deleted = %v, want [old1]", fv.deleted)
	}
}

func TestCleanRelationSupersedeArchivesAndCreates(t *testing.T) {
	old := &store.InterestPoint{ID: "old1", AgentID: "agent-a", Name: "旧观点", Status: "active"}
	st := newFakeStore()
	st.ip["old1"] = old
	fv := &fakeVec{}
	c := New(fakeEmbedder{}, fv, st, config.ForkConfig{})
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{
		vvRel("新观点", verify.RelationSupersede, "old1", "取代旧观点"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != "archived" {
		t.Errorf("old status = %q, want archived", old.Status)
	}
	if len(fv.deleted) != 1 || fv.deleted[0] != "old1" {
		t.Errorf("vec deleted = %v, want [old1]", fv.deleted)
	}
	if len(out) != 1 {
		t.Fatalf("created = %d, want 1 (supersede creates new)", len(out))
	}
	if out[0].ID == "old1" || out[0].Name != "新观点" {
		t.Errorf("new point = %+v", out[0])
	}
	// sequel edge old→new recorded.
	foundSequel := false
	for _, e := range st.edges {
		if e.SourceID == "old1" && e.TargetID == out[0].ID && e.Kind == store.EdgeSequel {
			foundSequel = true
		}
	}
	if !foundSequel {
		t.Errorf("missing sequel edge old→new; edges = %+v", st.edges)
	}
}

func TestCleanRelationUpdateMergesIntoOld(t *testing.T) {
	old := &store.InterestPoint{ID: "old1", AgentID: "agent-a", Name: "旧偏好",
		Importance: 0.5, SeenCount: 2, Keywords: []string{"old"}, Status: "active"}
	st := newFakeStore()
	st.ip["old1"] = old
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{
		vvRel("旧偏好更新", verify.RelationUpdate, "old1", "补充细节"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %d, want 1", len(out))
	}
	updated := st.ip["old1"]
	if updated == nil {
		t.Fatal("old point missing")
	}
	if updated.Status != "active" {
		t.Errorf("old status = %q, want active (update keeps it)", updated.Status)
	}
	if updated.SeenCount != 3 {
		t.Errorf("seen_count = %d, want 3", updated.SeenCount)
	}
	if updated.Importance <= 0.5 {
		t.Errorf("importance not boosted: %f", updated.Importance)
	}
	if out[0].ID != "old1" {
		t.Errorf("update output should reference old point id")
	}
}

func TestCleanSubjectivePropagatesToCreated(t *testing.T) {
	st := newFakeStore()
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})
	v := vv("喜欢 Go", 0.8)
	v.Subjective = true
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{v})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("created = %d, want 1", len(out))
	}
	if !out[0].Subjective {
		t.Error("subjective flag not propagated to interest point")
	}
}
