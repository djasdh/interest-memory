package wiki

import (
	"context"
	"testing"

	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
)

// namedEmbedder returns per-name fixed vectors so clustering is deterministic:
// "alpha"/"alpha2" are near each other, "beta" is far.
type namedEmbedder struct{}

func (namedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	switch text {
	case "alpha", "alpha2":
		return []float32{1, 0}, nil
	case "beta":
		return []float32{0, 1}, nil
	}
	return []float32{0.5, 0.5}, nil
}

func ip(id, name string) store.InterestPoint {
	return store.InterestPoint{ID: id, AgentID: "a", Name: name, Status: "active"}
}

func TestGroupByClusterPairsByEmbedding(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, err := vec.NewFallback(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	deps := ToolsDeps{Store: st, Vec: v, Embedder: namedEmbedder{}}

	// Fallback Vec.Get always returns nil, so groupByCluster must fall back to
	// Embed. alpha/alpha2 share a vector (>=0.6 groupSim pairs them); beta far.
	pts := []store.InterestPoint{ip("ip-a", "alpha"), ip("ip-a2", "alpha2"), ip("ip-b", "beta")}
	clusters, isolated, err := groupByCluster(context.Background(), deps, "a", pts, 0.6)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(clusters))
	}
	if len(clusters[0]) != 2 {
		t.Errorf("cluster size = %d, want 2 (alpha, alpha2)", len(clusters[0]))
	}
	if len(isolated) != 1 || isolated[0].Pt.ID != "ip-b" {
		t.Errorf("isolated = %+v, want [ip-b]", isolated)
	}
}

func TestGroupByClusterReusesStoredVectorViaGet(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, err := vec.NewSQLiteVec(st.DB(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Available() {
		t.Skip("sqlite-vec not available")
	}
	if err := v.Upsert(context.Background(), vec.Entry{ID: "ip-a", AgentID: "a", Kind: "interest_point", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := v.Upsert(context.Background(), vec.Entry{ID: "ip-b", AgentID: "a", Kind: "interest_point", Vector: []float32{0, 1}}); err != nil {
		t.Fatal(err)
	}
	deps := ToolsDeps{Store: st, Vec: v} // no Embedder: must use Get, never Embed
	pts := []store.InterestPoint{ip("ip-a", "alpha"), ip("ip-b", "beta")}
	clusters, isolated, err := groupByCluster(context.Background(), deps, "a", pts, 0.6)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Errorf("clusters = %d, want 0 (orthogonal vectors)", len(clusters))
	}
	if len(isolated) != 2 {
		t.Errorf("isolated = %d, want 2 (a and b vectors orthogonal, both isolated)", len(isolated))
	}
}
