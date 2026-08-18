package interest

import (
	"context"
	"testing"

	"github.com/djasdh/interest-memory/internal/vec"
)

// clusterVec returns scripted Search hits and Get vectors.
type clusterVec struct {
	searches map[string][]vec.Hit // keyed by agentID+"|"+tag
	entries  map[string]*vec.Entry
}

func (c *clusterVec) Search(_ context.Context, agentID string, q []float32, _ int) ([]vec.Hit, error) {
	if hits, ok := c.searches[agentID+"|"+tagOf(q)]; ok {
		return hits, nil
	}
	return nil, nil
}

func (c *clusterVec) Get(_ context.Context, _, id string) (*vec.Entry, error) {
	return c.entries[id], nil
}

func (c *clusterVec) Upsert(context.Context, vec.Entry) error { return nil }
func (c *clusterVec) Delete(context.Context, string, string) error {
	return nil
}

// tagOf maps a vector to a bucket key via its first element.
func tagOf(q []float32) string {
	if len(q) == 0 {
		return "v0"
	}
	return "v" + itoa(int(q[0]))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func TestClusterConnectsCurrentPoints(t *testing.T) {
	// Three current points; A↔B similar (>0.75), C isolated.
	pts := []Point{
		{Candidate: mergeCand("A", 0.9), Vec: []float32{1, 0}},
		{Candidate: mergeCand("B", 0.85), Vec: []float32{0.9, 0.1}},
		{Candidate: mergeCand("C", 0.8), Vec: []float32{0, 1}},
	}
	cv := &clusterVec{entries: map[string]*vec.Entry{}}
	res, err := Cluster(context.Background(), "ag", cv, nil, pts, 0.75, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Components) != 1 {
		t.Fatalf("components = %d, want 1 ({A,B})", len(res.Components))
	}
	if len(res.Components[0].Members) != 2 {
		t.Errorf("component members = %d, want 2", len(res.Components[0].Members))
	}
	if len(res.Isolated) != 1 || res.Isolated[0].Candidate.Topic != "C" {
		t.Errorf("isolated = %+v, want [C]", res.Isolated)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("conflicts = %+v, want none", res.Conflicts)
	}
}

func TestClusterConnectsToHistorical(t *testing.T) {
	pts := []Point{
		{Candidate: mergeCand("A", 0.9), Vec: []float32{1, 0}},
	}
	cv := &clusterVec{
		searches: map[string][]vec.Hit{
			"ag|v1": {{ID: "h1", AgentID: "ag", Kind: "interest_point", Score: 0.82}},
		},
		entries: map[string]*vec.Entry{
			"h1": {ID: "h1", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.9, 0.1}},
		},
	}
	res, err := Cluster(context.Background(), "ag", cv, nil, pts, 0.75, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Components) != 1 {
		t.Fatalf("components = %d, want 1", len(res.Components))
	}
	comp := res.Components[0]
	if len(comp.Members) != 1 || comp.Members[0].Candidate.Topic != "A" {
		t.Errorf("members = %+v, want [A]", comp.Members)
	}
	if len(comp.Hist) != 1 || comp.Hist[0].Pt.ID != "h1" {
		t.Errorf("hist = %+v, want [h1]", comp.Hist)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("conflicts = %+v, want none", res.Conflicts)
	}
}

func TestClusterConflictsQueue(t *testing.T) {
	// Current point M connects to two historical points H1, H2 that are NOT
	// similar to each other → competing sub-groups {M,H1} and {M,H2} form a
	// conflict queue.
	pts := []Point{
		{Candidate: mergeCand("M", 0.9), Vec: []float32{1, 0, 0}},
	}
	cv := &clusterVec{
		searches: map[string][]vec.Hit{
			"ag|v1": {
				{ID: "h1", AgentID: "ag", Kind: "interest_point", Score: 0.9},
				{ID: "h2", AgentID: "ag", Kind: "interest_point", Score: 0.9},
			},
		},
		entries: map[string]*vec.Entry{
			// H1, H2 both close to M (cos 0.9) but far from each other
			// (cos 0.65 < 0.8).
			"h1": {ID: "h1", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.9, 0.4, 0}},
			"h2": {ID: "h2", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.9, -0.4, 0}},
		},
	}
	res, err := Cluster(context.Background(), "ag", cv, nil, pts, 0.75, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflict queues = %d, want 1", len(res.Conflicts))
	}
	queue := res.Conflicts[0]
	if len(queue) != 2 {
		t.Fatalf("conflict sub-groups = %d, want 2 ({M,H1} and {M,H2})", len(queue))
	}
	for _, g := range queue {
		if len(g.Members) != 1 || g.Members[0].Candidate.Topic != "M" {
			t.Errorf("sub-group = %+v, want leader M", g)
		}
	}
	// Both groups size 2 → order stable.
	if len(res.Components) != 0 {
		t.Errorf("components = %+v, want empty (conflicted groups pulled out)", res.Components)
	}
}

func TestClusterNoConflictWhenHistSimilar(t *testing.T) {
	// M close to H1, H2, and H1 close to H2 → single flat component, no queue.
	pts := []Point{
		{Candidate: mergeCand("M", 0.9), Vec: []float32{1, 0}},
	}
	cv := &clusterVec{
		searches: map[string][]vec.Hit{
			"ag|v1": {
				{ID: "h1", AgentID: "ag", Kind: "interest_point", Score: 0.9},
				{ID: "h2", AgentID: "ag", Kind: "interest_point", Score: 0.9},
			},
		},
		entries: map[string]*vec.Entry{
			"h1": {ID: "h1", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.99, 0.1}},
			"h2": {ID: "h2", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.95, 0.2}},
		},
	}
	res, err := Cluster(context.Background(), "ag", cv, nil, pts, 0.75, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("conflicts = %+v, want none (H1≈H2)", res.Conflicts)
	}
	if len(res.Components) != 1 {
		t.Fatalf("components = %d, want 1 flat component", len(res.Components))
	}
	comp := res.Components[0]
	if len(comp.Hist) != 2 {
		t.Errorf("hist = %d, want 2 (H1+H2 in same component)", len(comp.Hist))
	}
}
