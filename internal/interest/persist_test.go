package interest

import (
	"context"
	"testing"

	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
)

// fakePersistStore records what Persist writes.
type fakePersistStore struct {
	upserted []store.InterestPoint
	edges    []store.Edge
	logs     []store.ChangeLog
	cons     []store.Contradiction
}

func (f *fakePersistStore) UpsertInterestPoint(_ context.Context, p store.InterestPoint) error {
	f.upserted = append(f.upserted, p)
	return nil
}
func (f *fakePersistStore) AddEdgePairs(_ context.Context, _ string, edges []store.Edge) error {
	f.edges = append(f.edges, edges...)
	return nil
}
func (f *fakePersistStore) AppendLog(_ context.Context, l store.ChangeLog) error {
	f.logs = append(f.logs, l)
	return nil
}
func (f *fakePersistStore) UpsertContradiction(_ context.Context, c store.Contradiction) error {
	f.cons = append(f.cons, c)
	return nil
}

// fakePersistVec records vector upserts/deletes.
type fakePersistVec struct {
	upserted []vec.Entry
	deleted  []string
}

func (f *fakePersistVec) Upsert(_ context.Context, e vec.Entry) error {
	f.upserted = append(f.upserted, e)
	return nil
}
func (f *fakePersistVec) Delete(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func adjFinal(action, id, name string, vecV []float32) FinalPoint {
	return FinalPoint{
		Point:  store.InterestPoint{ID: id, AgentID: "ag", Name: name, Status: "active"},
		Vec:    vecV,
		Action: action,
	}
}

func TestPersistCreatesAndUpdates(t *testing.T) {
	st := &fakePersistStore{}
	vi := &fakePersistVec{}
	adj := Adjudication{
		FinalPoints: []FinalPoint{
			adjFinal("create", "n1", "新点", []float32{1, 0}),
			adjFinal("update", "h1", "历史更新", []float32{0.9, 0.1}),
		},
	}
	if err := Persist(context.Background(), "ag", st, vi, adj, 0.50); err != nil {
		t.Fatal(err)
	}
	if len(st.upserted) != 2 {
		t.Fatalf("upserted = %d, want 2", len(st.upserted))
	}
	if len(vi.upserted) != 2 {
		t.Fatalf("vec upserted = %d, want 2", len(vi.upserted))
	}
	// Logs: create + update.
	actions := map[string]bool{}
	for _, l := range st.logs {
		actions[l.Action+"|"+l.EntityID] = true
	}
	if !actions["create|n1"] {
		t.Errorf("missing create log; logs = %+v", st.logs)
	}
	if !actions["update|h1"] {
		t.Errorf("missing update log; logs = %+v", st.logs)
	}
}

func TestPersistArchives(t *testing.T) {
	st := &fakePersistStore{}
	vi := &fakePersistVec{}
	adj := Adjudication{
		FinalPoints: []FinalPoint{
			adjFinal("create", "n1", "新点", []float32{1, 0}),
		},
		Archived: []ArchivedPoint{
			{Pt: store.InterestPoint{ID: "h1", AgentID: "ag", Name: "旧点", Status: "active"}},
		},
	}
	if err := Persist(context.Background(), "ag", st, vi, adj, 0.50); err != nil {
		t.Fatal(err)
	}
	// Archived point upserted with status=archived, vector deleted.
	foundArchived := false
	for _, p := range st.upserted {
		if p.ID == "h1" && p.Status == "archived" {
			foundArchived = true
		}
	}
	if !foundArchived {
		t.Errorf("archived point not upserted; upserted = %+v", st.upserted)
	}
	if len(vi.deleted) != 1 || vi.deleted[0] != "h1" {
		t.Errorf("vec deleted = %v, want [h1]", vi.deleted)
	}
	// Archive log.
	actions := map[string]bool{}
	for _, l := range st.logs {
		actions[l.Action+"|"+l.EntityID] = true
	}
	if !actions["archive|h1"] {
		t.Errorf("missing archive log; logs = %+v", st.logs)
	}
}

func TestPersistContradictions(t *testing.T) {
	st := &fakePersistStore{}
	vi := &fakePersistVec{}
	adj := Adjudication{
		Contradictions: []store.Contradiction{
			{AgentID: "ag", LeftID: "a", RightID: "b", Description: "矛盾", Status: "open"},
		},
	}
	if err := Persist(context.Background(), "ag", st, vi, adj, 0.50); err != nil {
		t.Fatal(err)
	}
	if len(st.cons) != 1 {
		t.Fatalf("contradictions = %d, want 1", len(st.cons))
	}
	// Bidirectional contradicts edge.
	var contraEdge bool
	for _, e := range st.edges {
		if e.Kind == store.EdgeContradict && e.SourceID == "a" && e.TargetID == "b" {
			contraEdge = true
		}
	}
	if !contraEdge {
		t.Errorf("missing contradicts edge; edges = %+v", st.edges)
	}
}

func TestPersistGeneratesRelated(t *testing.T) {
	st := &fakePersistStore{}
	vi := &fakePersistVec{}
	// n1 (1,0) and h1 (0.9,0.1): cos ≈ 0.994 ≥ 0.50 → related edge.
	adj := Adjudication{
		FinalPoints: []FinalPoint{
			adjFinal("create", "n1", "新点", []float32{1, 0}),
			adjFinal("update", "h1", "历史更新", []float32{0.9, 0.1}),
		},
	}
	if err := Persist(context.Background(), "ag", st, vi, adj, 0.50); err != nil {
		t.Fatal(err)
	}
	var related bool
	for _, e := range st.edges {
		if e.Kind == store.EdgeRelated {
			related = true
			if e.Weight <= 0.5 {
				t.Errorf("related weight = %f, want ~0.994", e.Weight)
			}
		}
	}
	if !related {
		t.Errorf("missing related edge; edges = %+v", st.edges)
	}
}

func TestPersistSkipsRelatedBelowThreshold(t *testing.T) {
	st := &fakePersistStore{}
	vi := &fakePersistVec{}
	// n1 (1,0) and h1 (0,1): cos = 0 < 0.50 → no related edge.
	adj := Adjudication{
		FinalPoints: []FinalPoint{
			adjFinal("create", "n1", "新点", []float32{1, 0}),
			adjFinal("update", "h1", "历史更新", []float32{0, 1}),
		},
	}
	if err := Persist(context.Background(), "ag", st, vi, adj, 0.50); err != nil {
		t.Fatal(err)
	}
	for _, e := range st.edges {
		if e.Kind == store.EdgeRelated {
			t.Errorf("unexpected related edge %+v (cos below threshold)", e)
		}
	}
}
