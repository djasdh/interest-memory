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
	// User's example: current point a similar to a1 & b1; current point b
	// similar to b1 & c1; a and b NOT similar to each other.
	//  → two independent components {a,a1,b1} and {b,b1,c1}, sharing b1.
	//  → one conflict queue of 2 components (b1 shared), pulled out of
	//    Components. Ordered by b1↔leader affinity: b1-a (0.862) > b1-b
	//    (0.814), so the a-component comes first.
	a := Point{Candidate: mergeCand("a", 0.9), Vec: []float32{1, 0, 0}}
	b := Point{Candidate: mergeCand("b", 0.85), Vec: []float32{0.4, 0.9, 0}}
	pts := []Point{a, b}
	cv := &clusterVec{
		searches: map[string][]vec.Hit{
			"ag|v1": {
				{ID: "a1", AgentID: "ag", Kind: "interest_point", Score: 0.9},
				{ID: "b1", AgentID: "ag", Kind: "interest_point", Score: 0.9},
			},
			"ag|v0": {
				{ID: "b1", AgentID: "ag", Kind: "interest_point", Score: 0.9},
				{ID: "c1", AgentID: "ag", Kind: "interest_point", Score: 0.9},
			},
		},
		entries: map[string]*vec.Entry{
			"a1": {ID: "a1", AgentID: "ag", Kind: "interest_point", Vector: []float32{1, 0, 0}, Metadata: map[string]string{"title": "a1"}},
			"b1": {ID: "b1", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.85, 0.5, 0}, Metadata: map[string]string{"title": "b1"}},
			"c1": {ID: "c1", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.4, 0.9, 0}, Metadata: map[string]string{"title": "c1"}},
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
		t.Fatalf("conflict queue size = %d, want 2 components sharing b1", len(queue))
	}
	// Components pulled out: conflicted ones must not also appear as plain
	// components.
	if len(res.Components) != 0 {
		t.Errorf("components = %+v, want empty (conflicted pulled out)", res.Components)
	}
	// Shared historical point b1 present in every queue component's Hist.
	for _, comp := range queue {
		found := false
		for _, h := range comp.Hist {
			if h.Pt.ID == "b1" {
				found = true
			}
		}
		if !found {
			t.Errorf("component %+v missing shared b1 in Hist", comp)
		}
	}
	// Order: b1-a affinity (0.862) > b1-b (0.814) → a-component first.
	if queue[0].Members[0].Candidate.Topic != "a" {
		t.Errorf("queue[0] leader = %q, want a (higher b1 affinity first)", queue[0].Members[0].Candidate.Topic)
	}
	if queue[1].Members[0].Candidate.Topic != "b" {
		t.Errorf("queue[1] leader = %q, want b", queue[1].Members[0].Candidate.Topic)
	}
}

func TestClusterNoConflictWhenHistSameComponent(t *testing.T) {
	// M similar to H1 and H2 → they form ONE component {M,H1,H2}, no conflict
	// queue (a single current point may ride multiple historical points).
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
			"h1": {ID: "h1", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.99, 0.1}, Metadata: map[string]string{"title": "h1"}},
			"h2": {ID: "h2", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.95, 0.2}, Metadata: map[string]string{"title": "h2"}},
		},
	}
	res, err := Cluster(context.Background(), "ag", cv, nil, pts, 0.75, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("conflicts = %+v, want none (H1,H2 ride one component)", res.Conflicts)
	}
	if len(res.Components) != 1 {
		t.Fatalf("components = %d, want 1 flat component", len(res.Components))
	}
	comp := res.Components[0]
	if len(comp.Hist) != 2 {
		t.Errorf("hist = %d, want 2 (H1+H2 in same component)", len(comp.Hist))
	}
	if len(res.Isolated) != 0 {
		t.Errorf("isolated = %+v, want none", res.Isolated)
	}
}
