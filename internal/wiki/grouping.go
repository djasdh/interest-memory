package wiki

import (
	"context"
	"fmt"
	"math"

	"github.com/djasdh/interest-memory/internal/store"
)

// pointWithVec is one interest point paired with its embedding for clustering.
type pointWithVec struct {
	Pt  store.InterestPoint
	Vec []float32
}

// groupByCluster runs the wikiloop EBD grouping: points whose pairwise cosine
// >= groupSim join one write cluster; points with no similar partner go to
// isolated. Grouping only — never merges (merging already happened during V1.3
// persist). Vectors are reused from the vec index (V1.3 upserted them) via
// Vec.Get; when Get yields nil (e.g. keyword-only Fallback) it falls back to
// embedding the point name. Points with neither path keep a nil vector and are
// treated as isolated (they still get their own loop).
func groupByCluster(ctx context.Context, deps ToolsDeps, agentID string, pts []store.InterestPoint, groupSim float64) (clusters [][]pointWithVec, isolated []pointWithVec, err error) {
	if groupSim <= 0 {
		groupSim = 0.75
	}
	n := len(pts)
	if n == 0 {
		return nil, nil, nil
	}

	pw := make([]pointWithVec, n)
	for i := range pts {
		p := pts[i]
		var v []float32
		if deps.Vec != nil {
			if ent, gerr := deps.Vec.Get(ctx, agentID, p.ID); gerr != nil {
				return nil, nil, fmt.Errorf("wiki: group: get vector %s: %w", p.ID, gerr)
			} else if ent != nil {
				v = ent.Vector
			}
		}
		if v == nil && deps.Embedder != nil {
			if ev, eerr := deps.Embedder.Embed(ctx, p.Name); eerr == nil {
				v = ev
			}
		}
		pw[i] = pointWithVec{Pt: p, Vec: v}
	}

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

	for i := 0; i < n; i++ {
		if pw[i].Vec == nil {
			continue
		}
		for j := i + 1; j < n; j++ {
			if pw[j].Vec == nil {
				continue
			}
			if cosine(pw[i].Vec, pw[j].Vec) >= groupSim {
				union(i, j)
			}
		}
	}

	groups := make(map[int][]pointWithVec)
	var order []int
	for i := range pw {
		r := find(i)
		if _, ok := groups[r]; !ok {
			order = append(order, r)
		}
		groups[r] = append(groups[r], pw[i])
	}
	for _, r := range order {
		if len(groups[r]) == 1 {
			isolated = append(isolated, groups[r][0])
			continue
		}
		clusters = append(clusters, groups[r])
	}
	return clusters, isolated, nil
}

// cosine returns the cosine similarity of two vectors (0 when either is empty
// or dimension-mismatched).
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
