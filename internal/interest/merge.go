package interest

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/djasdh/interest-memory/internal/fork"
	"github.com/djasdh/interest-memory/internal/llm"
)

// ClusterLLM is the chat surface s1's per-cluster merge judgment needs
// (implemented by *llm.Client). Narrow for test fakes.
type ClusterLLM interface {
	ChatJSON(ctx context.Context, messages []llm.Message, out any) error
}

// Point is a deduped/merged interest point produced by DedupeMerge (s1) and
// consumed by Cluster (s2). Vec is the candidate's embedding, computed once
// and reused so s2 never re-embeds the same text.
type Point struct {
	Candidate fork.Candidate
	Vec       []float32
}

// mergeVerdict is one per-cluster LLM verdict: how the source candidates in a
// similarity cluster should be combined.
type mergeVerdict struct {
	Action       string         `json:"action"` // "merge" | "compose" | "keep"
	SourceTopics []string       `json:"source_topics"`
	Merged       mergeCandidate `json:"merged"`
}

type mergeCandidate struct {
	Topic      string   `json:"topic"`
	Reason     string   `json:"reason"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	TurnRange  [2]int   `json:"turn_range"`
	Subjective bool     `json:"subjective"`
	WikiWorthy *bool    `json:"wiki_worthy,omitempty"`
}

// DedupeMerge is pipeline stage s1: fold identical topics (string-normalized)
// for free, cluster remaining candidates by embedding similarity (> clusterSim
// pairs), and ask the LLM once per cluster how to merge/keep its members.
// Returns the merged interest points with their embeddings. Never persists.
// Embedding and per-cluster LLM calls run in parallel (maxConc workers,
// fail-fast on the first error), while the output order matches the serial
// pipeline (input order / cluster order).
func DedupeMerge(ctx context.Context, agentID string, em Embedder, cl ClusterLLM, clusterSim float64, maxConc int, cands []fork.Candidate) ([]Point, error) {
	if clusterSim <= 0 {
		clusterSim = 0.6
	}
	if maxConc <= 0 {
		maxConc = 4
	}
	// ① string-level dedup (case/whitespace normalized).
	deduped := dedupeCandidates(cands)
	if len(deduped) == 0 {
		return nil, nil
	}

	// ② embed each surviving candidate once, in parallel (index-preserving).
	pts := make([]Point, len(deduped))
	if err := runParallel(ctx, len(deduped), maxConc, func(ctx context.Context, i int) error {
		v, err := em.Embed(ctx, candidateText(deduped[i]))
		if err != nil {
			return fmt.Errorf("interest: dedupe-merge embed: %w", err)
		}
		pts[i] = Point{Candidate: deduped[i], Vec: v}
		return nil
	}); err != nil {
		return nil, err
	}

	// ③ cluster by pairwise similarity > clusterSim.
	clusters := connectedComponents(pts, clusterSim)

	// ④ one LLM call per cluster, in parallel; isolated points pass through
	// unchanged. Results are index-preserved then concatenated in cluster
	// order (identical to the serial version).
	results := make([][]Point, len(clusters))
	if err := runParallel(ctx, len(clusters), maxConc, func(ctx context.Context, i int) error {
		grp := clusters[i]
		if len(grp) == 1 {
			results[i] = []Point{grp[0]}
			return nil
		}
		merged, err := mergeCluster(ctx, em, cl, grp)
		if err != nil {
			return err
		}
		results[i] = merged
		return nil
	}); err != nil {
		return nil, err
	}
	var out []Point
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

// runParallel invokes fn(i) for i in [0,n) with up to maxConc concurrent
// workers, canceling the rest on the first error (fail-fast). Each fn receives
// a context canceled as soon as any worker fails.
func runParallel(ctx context.Context, n, maxConc int, fn func(ctx context.Context, i int) error) error {
	if n == 0 {
		return nil
	}
	if maxConc <= 0 {
		maxConc = 4
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		first error
		wg    sync.WaitGroup
	)
	sem := make(chan struct{}, maxConc)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return first
}

// candidateText is the embeddable text for a candidate (topic + reason).
func candidateText(c fork.Candidate) string {
	if c.Reason == "" {
		return c.Topic
	}
	return c.Topic + "\n" + c.Reason
}

// connectedComponents groups points whose pairwise cosine similarity exceeds
// threshold (union-find over the "similar" relation). Isolated points are
// single-member components, in input order.
func connectedComponents(pts []Point, threshold float64) [][]Point {
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
	for i := range pts {
		for j := i + 1; j < n; j++ {
			if cosine(pts[i].Vec, pts[j].Vec) > threshold {
				union(i, j)
			}
		}
	}
	groups := make(map[int][]int)
	var order []int
	for i := range pts {
		r := find(i)
		if _, ok := groups[r]; !ok {
			order = append(order, r)
		}
		groups[r] = append(groups[r], i)
	}
	out := make([][]Point, 0, len(order))
	for _, r := range order {
		var comp []Point
		for _, i := range groups[r] {
			comp = append(comp, pts[i])
		}
		out = append(out, comp)
	}
	return out
}

// mergeCluster sends one cluster's candidates to the LLM and applies the
// verdict: merge folds sources into the merged candidate, compose emits the
// fused candidate, keep emits each source unchanged.
func mergeCluster(ctx context.Context, em Embedder, cl ClusterLLM, grp []Point) ([]Point, error) {
	prompt := buildMergePrompt(grp)
	var verdicts []mergeVerdict
	if err := cl.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &verdicts); err != nil {
		return nil, fmt.Errorf("interest: dedupe-merge llm: %w", err)
	}

	var out []Point
	consumed := make(map[string]bool)
	for _, vd := range verdicts {
		for _, s := range vd.SourceTopics {
			consumed[s] = true
		}
		switch vd.Action {
		case "merge", "compose":
			c := mergeCandidateToFork(vd.Merged)
			vec, err := em.Embed(ctx, candidateText(c))
			if err != nil {
				return nil, fmt.Errorf("interest: dedupe-merge re-embed: %w", err)
			}
			out = append(out, Point{Candidate: c, Vec: vec})
		case "keep":
			// Emit the source point(s) unchanged, in cluster order.
			for _, p := range grp {
				if containsString(vd.SourceTopics, p.Candidate.Topic) {
					out = append(out, p)
				}
			}
		}
	}
	// Sources never mentioned by any verdict pass through unchanged.
	for _, p := range grp {
		if !consumed[p.Candidate.Topic] {
			out = append(out, p)
		}
	}
	return out, nil
}

func buildMergePrompt(cl []Point) string {
	var b strings.Builder
	b.WriteString("You are merging interest points extracted from a conversation. Some of the following points describe the same or closely-related topics.\n\n")
	for i, p := range cl {
		b.WriteString(fmt.Sprintf("%d. topic: %s\n   reason: %s\n   confidence: %.2f\n   tags: %s\n",
			i+1, p.Candidate.Topic, p.Candidate.Reason, p.Candidate.Confidence, strings.Join(p.Candidate.Tags, ", ")))
	}
	b.WriteString(`
Decide for each point how it relates to the others. Return ONLY valid JSON, no other text, an array of verdict objects:
[
  {
    "action": "merge" | "compose" | "keep",
    "source_topics": ["<exact topic strings being combined>"],
    "merged": { "topic": "...", "reason": "...", "confidence": 0.0-1.0, "tags": [...], "turn_range": [0,0], "subjective": false }
  }
]

Rules:
- "merge": points are the SAME topic in different wording → fold into one merged object.
- "compose": points are RELATED but distinct → fuse into a new combined candidate.
- "keep": a point is distinct and should stay as-is → verdict with action "keep" and source_topics=[its exact topic].
Every point must appear in exactly one verdict.`)
	return b.String()
}

func mergeCandidateToFork(m mergeCandidate) fork.Candidate {
	return fork.Candidate{
		Topic:      m.Topic,
		Reason:     m.Reason,
		Confidence: m.Confidence,
		Tags:       m.Tags,
		TurnRange:  m.TurnRange,
		Subjective: m.Subjective,
		WikiWorthy: m.WikiWorthy,
	}
}

// dedupeCandidates folds string-identical topics (case/whitespace normalized),
// keeping the highest-confidence variant and merging turn range / tags / first
// wiki_worthy verdict — mirroring fork.dedupe's semantics.
func dedupeCandidates(cands []fork.Candidate) []fork.Candidate {
	var out []fork.Candidate
	for _, c := range cands {
		key := normalizeTopic(c.Topic)
		if key == "" {
			out = append(out, c)
			continue
		}
		idx := -1
		for i := range out {
			if normalizeTopic(out[i].Topic) == key {
				idx = i
				break
			}
		}
		if idx == -1 {
			out = append(out, c)
			continue
		}
		m := out[idx]
		if c.Confidence > m.Confidence {
			m.Topic = c.Topic
			m.Reason = c.Reason
			m.Subjective = c.Subjective
			m.Confidence = c.Confidence
		}
		if c.TurnRange[0] > 0 && (m.TurnRange[0] == 0 || c.TurnRange[0] < m.TurnRange[0]) {
			m.TurnRange[0] = c.TurnRange[0]
		}
		if c.TurnRange[1] > m.TurnRange[1] {
			m.TurnRange[1] = c.TurnRange[1]
		}
		if m.WikiWorthy == nil && c.WikiWorthy != nil {
			m.WikiWorthy = c.WikiWorthy
		}
		for _, tag := range c.Tags {
			if !containsString(m.Tags, tag) {
				m.Tags = append(m.Tags, tag)
			}
		}
		out[idx] = m
	}
	return out
}

func normalizeTopic(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

// cosine returns cosine similarity of two vectors (0 for degenerate or
// mismatched lengths).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
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
