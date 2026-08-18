package interest

import (
	"context"
	"fmt"

	"github.com/djasdh/interest-memory/internal/store"
)

// HistPoint is a historical interest point joined into a component because it
// is similar to a current-point leader. Pt carries the full record; Vec the
// stored embedding fetched via VectorIndex.Get.
type HistPoint struct {
	Pt  store.InterestPoint
	Vec []float32
}

// Component is one connected component of similar points (the unit of V1.2's
// per-group LLM adjudication). Members are current points (cluster leaders);
// Hist are historical points similar to ≥1 member.
type Component struct {
	Members []Point
	Hist    []HistPoint
}

// size returns the total node count (members + hist) used to order conflict
// queues smallest-first.
func (c Component) size() int { return len(c.Members) + len(c.Hist) }

// ClusterResult is s2's output: connected components, isolated current points
// (no similar partner), and conflict queues — competing groups that share a
// current point and must be adjudicated in order (smaller component first).
type ClusterResult struct {
	Components []Component
	Isolated   []Point
	Conflicts  [][]Component
}

// Cluster is pipeline stage s2: build pairwise similarity pairs among current
// points (> mergeSim) and between each current point and historical interest
// points (> histSim, via vec.Search + vec.Get for the exact vector), then
// group into connected components.
//
// A conflict arises when one current point M is similar to two or more
// historical points H1, H2 that are NOT similar to each other: M cannot merge
// into both, so the competing sub-groups {M,H1}, {M,H2} are pulled out of the
// flat component list into a conflict queue (smaller component adjudicated
// first). Everything else forms plain components; current points with no
// similar partner at all are Isolated. Never persists and never calls the LLM.
func Cluster(ctx context.Context, agentID string, vi VectorIndex, st Store, pts []Point, mergeSim, histSim float64) (ClusterResult, error) {
	if mergeSim <= 0 {
		mergeSim = 0.75
	}
	if histSim <= 0 {
		histSim = 0.8
	}
	n := len(pts)

	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	// Current↔current edges (> mergeSim).
	for i := range pts {
		for j := i + 1; j < n; j++ {
			if cosine(pts[i].Vec, pts[j].Vec) > mergeSim {
				union(i, j)
			}
		}
	}

	// Current↔historical edges (> histSim): search per current point, fetch
	// the stored vector via Get, recompute exact cosine.
	type histEdge struct {
		pt  int
		hp  HistPoint
		sim float64
	}
	var histEdges []histEdge
	if vi != nil {
		for i := range pts {
			hits, err := vi.Search(ctx, agentID, pts[i].Vec, 20)
			if err != nil {
				return ClusterResult{}, fmt.Errorf("interest: cluster search: %w", err)
			}
			for _, h := range hits {
				if h.Kind != "interest_point" {
					continue
				}
				if float64(h.Score) < histSim {
					break // ranked by score — later hits can't recover
				}
				ent, err := vi.Get(ctx, agentID, h.ID)
				if err != nil {
					return ClusterResult{}, fmt.Errorf("interest: cluster get %s: %w", h.ID, err)
				}
				if ent == nil {
					continue // stale vector (archived after indexing)
				}
				sim := cosine(pts[i].Vec, ent.Vector)
				if sim <= histSim {
					continue
				}
				hp := HistPoint{
					Pt:  store.InterestPoint{ID: ent.ID, AgentID: ent.AgentID, Name: ent.Metadata["title"]},
					Vec: ent.Vector,
				}
				if st != nil {
					if p, err := st.GetInterestPoint(ctx, agentID, ent.ID); err == nil && p != nil {
						hp.Pt = *p
					}
				}
				histEdges = append(histEdges, histEdge{pt: i, hp: hp, sim: sim})
				// Historical points are leaves in the current↔current union:
				// they never union with each other directly.
			}
		}
	}

	// Group histEdges per current point to detect conflicts.
	histOf := make(map[int][]histEdge)
	for _, e := range histEdges {
		histOf[e.pt] = append(histOf[e.pt], e)
	}

	// Detect conflict current points: ≥2 historical neighbors that are not
	// mutually similar. Their sub-groups are adjudicated separately.
	conflicted := make(map[int]bool)
	for i, edges := range histOf {
		if len(edges) < 2 {
			continue
		}
		// Two hist neighbors H1, H2 are "in conflict" if their own similarity
		// is below histSim (they are not one same-topic group).
		conflicted[i] = true
		for a := 0; a < len(edges); a++ {
			for b := a + 1; b < len(edges); b++ {
				if cosine(edges[a].hp.Vec, edges[b].hp.Vec) > histSim {
					// H1 and H2 are themselves similar → not a conflict, they
					// belong to one group.
					conflicted[i] = false
					break
				}
			}
		}
	}

	var res ClusterResult
	// Build components from union-find roots.
	groups := make(map[int][]int)
	var order []int
	for i := range pts {
		r := find(i)
		if _, ok := groups[r]; !ok {
			order = append(order, r)
		}
		groups[r] = append(groups[r], i)
	}

	for _, r := range order {
		idx := groups[r]
		if len(idx) == 1 {
			i := idx[0]
			if len(histOf[i]) == 0 {
				res.Isolated = append(res.Isolated, pts[i])
				continue
			}
			if conflicted[i] {
				// Pull each {M,Hk} pair into one conflict queue instead of a
				// single flat component.
				var queue []Component
				for _, e := range histOf[i] {
					queue = append(queue, Component{Members: []Point{pts[i]}, Hist: []HistPoint{e.hp}})
				}
				res.Conflicts = append(res.Conflicts, queue)
				continue
			}
		}
		var comp Component
		for _, i := range idx {
			comp.Members = append(comp.Members, pts[i])
		}
		for _, i := range idx {
			for _, e := range histOf[i] {
				comp.Hist = append(comp.Hist, e.hp)
			}
		}
		res.Components = append(res.Components, comp)
	}

	// Sort each conflict queue's sub-groups smallest-first (stable).
	for i := range res.Conflicts {
		sortComponentsBySize(res.Conflicts[i])
	}
	return res, nil
}

// sortComponentsBySize orders components smallest-first (stable for equal
// sizes, preserving insertion order). The user's rule: smaller cluster is
// adjudicated first.
func sortComponentsBySize(comps []Component) {
	for i := 1; i < len(comps); i++ {
		for j := i; j > 0 && comps[j-1].size() > comps[j].size(); j-- {
			comps[j-1], comps[j] = comps[j], comps[j-1]
		}
	}
}
