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
// Hist are historical points similar to ≥1 member. MemberHist preserves the
// per-member association (member topic → the historical points it is similar
// to) so V1.2 can adjudicate each current↔historical pair explicitly.
type Component struct {
	Members    []Point
	Hist       []HistPoint
	MemberHist map[string][]HistPoint
}

// ClusterResult is s2's output: connected components, isolated current points
// (no similar partner), and conflict queues — components that share a
// historical point and must be adjudicated in order (highest shared-point
// affinity first). Conflict components are removed from Components and appear
// only in their queue.
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
// A conflict arises when a historical point H is shared by two or more
// components (each component's current-point leader is similar to H): the
// components compete for H, so they are pulled out of the flat component list
// into a conflict queue, ordered by H's affinity to each component's leader
// (highest first — adjudicated first). Everything else forms plain components;
// current points with no similar partner at all are Isolated. Never persists
// and never calls the LLM.
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

	// Group histEdges per current point (a current point rides all its
	// similar historical points in one component).
	histOf := make(map[int][]histEdge)
	for _, e := range histEdges {
		histOf[e.pt] = append(histOf[e.pt], e)
	}

	var res ClusterResult
	// Connected components over current points.
	groups := make(map[int][]int)
	var order []int
	for i := range pts {
		r := find(i)
		if _, ok := groups[r]; !ok {
			order = append(order, r)
		}
		groups[r] = append(groups[r], i)
	}

	var comps []Component
	for _, r := range order {
		idx := groups[r]
		if len(idx) == 1 {
			i := idx[0]
			if len(histOf[i]) == 0 {
				// Genuinely isolated: no current partner, no historical match.
				res.Isolated = append(res.Isolated, pts[i])
				continue
			}
		}
		var comp Component
		comp.MemberHist = make(map[string][]HistPoint)
		for _, i := range idx {
			comp.Members = append(comp.Members, pts[i])
		}
		for _, i := range idx {
			for _, e := range histOf[i] {
				comp.Hist = append(comp.Hist, e.hp)
				comp.MemberHist[pts[i].Candidate.Topic] = append(comp.MemberHist[pts[i].Candidate.Topic], e.hp)
			}
		}
		comps = append(comps, comp)
	}

	// Conflict detection: a historical point shared by ≥2 components means the
	// components compete for it. Pull those components into a conflict queue
	// (removed from the plain list) and order each queue by the affinity of the
	// shared point to each component's leader — higher affinity adjudicated
	// first.
	histInComps := make(map[string][]int)
	for ci, comp := range comps {
		for _, h := range comp.Hist {
			histInComps[h.Pt.ID] = append(histInComps[h.Pt.ID], ci)
		}
	}
	used := make([]bool, len(comps))
	for sharedID, ciList := range histInComps {
		if len(ciList) < 2 {
			continue
		}
		var sharedVec []float32
		for _, ci := range ciList {
			for _, h := range comps[ci].Hist {
				if h.Pt.ID == sharedID {
					sharedVec = h.Vec
				}
			}
		}
		var queue []Component
		for _, ci := range ciList {
			if used[ci] {
				continue
			}
			used[ci] = true
			queue = append(queue, comps[ci])
		}
		if len(queue) >= 2 {
			sortConflictQueue(queue, sharedVec)
			res.Conflicts = append(res.Conflicts, queue)
		}
	}
	for ci, comp := range comps {
		if !used[ci] {
			res.Components = append(res.Components, comp)
		}
	}
	return res, nil
}

// affinityOf returns the highest similarity between a shared historical vector
// and any current-point (leader) vector in the component.
func affinityOf(comp Component, sharedVec []float32) float64 {
	best := -1.0
	for _, m := range comp.Members {
		if s := cosine(m.Vec, sharedVec); s > best {
			best = s
		}
	}
	return best
}

// sortConflictQueue orders a conflict queue by each component's affinity to
// the shared historical point, highest first (the component whose leader is
// closest to the shared point is adjudicated first). Stable for ties.
func sortConflictQueue(queue []Component, sharedVec []float32) {
	for i := 1; i < len(queue); i++ {
		for j := i; j > 0 && affinityOf(queue[j-1], sharedVec) < affinityOf(queue[j], sharedVec); j-- {
			queue[j-1], queue[j] = queue[j], queue[j-1]
		}
	}
}
