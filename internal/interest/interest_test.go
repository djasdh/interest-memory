package interest

import (
	"context"
	"testing"
	"time"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/fork"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/verify"
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
	logs   []store.ChangeLog
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
func (f *fakeStore) AppendLog(_ context.Context, l store.ChangeLog) error {
	f.logs = append(f.logs, l)
	return nil
}

func vv(topic string, conf float64) verify.Verified {
	return verify.Verified{
		Candidate:   fork.Candidate{Topic: topic, Reason: "reason for " + topic, Confidence: conf, Tags: []string{"t1"}},
		Reliability: store.Reliability{Confidence: conf, Status: "supported"},
		Freshness:   store.Freshness{Level: "fresh"},
	}
}

func TestInterestLogsOnChanges(t *testing.T) {
	old := &store.InterestPoint{ID: "old1", AgentID: "a", Name: "旧观点", Status: "active"}
	st := newFakeStore()
	st.ip["old1"] = old
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})

	// create
	if _, _, err := c.Clean(context.Background(), "a", []verify.Verified{vv("新主题", 0.9)}); err != nil {
		t.Fatal(err)
	}
	// update (merge into existing via high similarity)
	st.ip["merge-target"] = &store.InterestPoint{ID: "merge-target", AgentID: "a", Name: "旧主题", Status: "active"}
	c2 := New(fakeEmbedder{}, &fakeVec{hits: []vec.Hit{{ID: "merge-target", Kind: "interest_point", Score: 0.95}}}, st, config.ForkConfig{})
	if _, _, err := c2.Clean(context.Background(), "a", []verify.Verified{vv("旧主题更新", 0.9)}); err != nil {
		t.Fatal(err)
	}
	// archive (delete relation)
	if _, _, err := c2.Clean(context.Background(), "a", []verify.Verified{vvRel("新观点", verify.RelationDelete, "old1", "")}); err != nil {
		t.Fatal(err)
	}

	actions := map[string]string{}
	for _, l := range st.logs {
		actions[l.Action+"|"+l.EntityID] = l.Title
	}
	if actions["create|"+newID("新主题")] != "新主题" {
		t.Errorf("missing create log; logs = %+v", st.logs)
	}
	if actions["update|merge-target"] != "旧主题" {
		t.Errorf("missing update log; logs = %+v", st.logs)
	}
	if actions["archive|old1"] != "旧观点" {
		t.Errorf("missing archive log; logs = %+v", st.logs)
	}
}

func TestInterestLogsSupersedeWithSequelEdge(t *testing.T) {
	old := &store.InterestPoint{ID: "old1", AgentID: "a", Name: "旧观点", Status: "active"}
	st := newFakeStore()
	st.ip["old1"] = old
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})
	if _, _, err := c.Clean(context.Background(), "a", []verify.Verified{
		vvRel("新观点", verify.RelationSupersede, "old1", ""),
	}); err != nil {
		t.Fatal(err)
	}
	var supersede *store.ChangeLog
	for i := range st.logs {
		if st.logs[i].Action == "supersede" {
			supersede = &st.logs[i]
		}
	}
	if supersede == nil {
		t.Fatalf("no supersede log; logs = %+v", st.logs)
	}
	if len(supersede.Edges) != 1 || supersede.Edges[0].Kind != store.EdgeSequel || supersede.Edges[0].SourceID != "old1" {
		t.Errorf("supersede edges = %+v", supersede.Edges)
	}
}

func TestCleanStoresEventTime(t *testing.T) {
	st := newFakeStore()
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})
	v := vv("带时间的主题", 0.8)
	et := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	v.Candidate.EventTime = et
	out, _, err := c.Clean(context.Background(), "agent-a", []verify.Verified{v})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].EventTime.Equal(et) {
		t.Errorf("event_time = %v, want %v", out[0].EventTime, et)
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

func TestCleanPropagatesWikiWorthy(t *testing.T) {
	f, tf := false, true
	st := newFakeStore()
	c := New(fakeEmbedder{}, &fakeVec{}, st, config.ForkConfig{})

	// create propagates the LLM verdict (false).
	v := vv("点A", 0.9)
	v.Candidate.WikiWorthy = &f
	if _, _, err := c.Clean(context.Background(), "a", []verify.Verified{v}); err != nil {
		t.Fatal(err)
	}
	if st.upsert == nil || st.upsert.WikiWorthy == nil || *st.upsert.WikiWorthy {
		t.Errorf("created wiki_worthy = %+v, want false", st.upsert.WikiWorthy)
	}

	// merge overwrites with a newer verdict (true).
	id := st.upsert.ID
	c2 := New(fakeEmbedder{}, &fakeVec{hits: []vec.Hit{{ID: id, Kind: "interest_point", Score: 0.95}}}, st, config.ForkConfig{})
	v2 := vv("点A", 0.9)
	v2.Candidate.WikiWorthy = &tf
	if _, _, err := c2.Clean(context.Background(), "a", []verify.Verified{v2}); err != nil {
		t.Fatal(err)
	}
	if st.upsert.WikiWorthy == nil || !*st.upsert.WikiWorthy {
		t.Errorf("merged wiki_worthy = %+v, want true", st.upsert.WikiWorthy)
	}

	// merge keeps the existing verdict when the new candidate has none.
	v3 := vv("点A", 0.9)
	if _, _, err := c2.Clean(context.Background(), "a", []verify.Verified{v3}); err != nil {
		t.Fatal(err)
	}
	if st.upsert.WikiWorthy == nil || !*st.upsert.WikiWorthy {
		t.Errorf("merged wiki_worthy = %+v, want retained true", st.upsert.WikiWorthy)
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
