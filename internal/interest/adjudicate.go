package interest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/store"
)

// FinalPoint is one interest point that a V1.2 adjudication decided to
// create/update/archive, ready for V1.3 to persist (with its embedding).
type FinalPoint struct {
	Point  store.InterestPoint
	Vec    []float32
	Action string // create | update | archive
}

// ArchivedPoint is a historical point the adjudication decided to archive.
type ArchivedPoint struct {
	Pt store.InterestPoint
}

// Adjudication is V1.2's output: the points with real changes (create/update)
// plus archived historical points and contradiction pairs. It never persists.
type Adjudication struct {
	FinalPoints    []FinalPoint
	Archived       []ArchivedPoint
	Contradictions []store.Contradiction
}

// adjudicateMeta is the reliability/freshness metadata the LLM assigns to a
// final point (a component decision's merged, or an isolated point's meta).
type adjudicateMeta struct {
	Subjective        bool    `json:"subjective"`
	ReliabilityStatus string  `json:"reliability_status"`
	Confidence        float64 `json:"confidence"`
	FreshnessLevel    string  `json:"freshness_level"`
	TTLDays           int     `json:"ttl_days"`
	WikiWorthy        *bool   `json:"wiki_worthy"`
}

// adjudicateDecision is one per-member verdict inside a component.
type adjudicateDecision struct {
	SourceTopic string             `json:"source_topic"`
	Action      string             `json:"action"` // merge | keep | archive
	TargetID    string             `json:"target_id"`
	Merged      mergeCandidate     `json:"merged"`
	Updates     []adjudicateUpdate `json:"updates"`
}

// adjudicateUpdate is a historical point the LLM decides to update as a
// side-effect of a member's keep/merge (e.g. Go 1.18 → 1.19). target_id names
// the historical point; merged is its post-update form.
type adjudicateUpdate struct {
	TargetID string         `json:"target_id"`
	Merged   mergeCandidate `json:"merged"`
}

type adjudicateContradiction struct {
	Left        string `json:"left"` // member topic or historical point id
	Right       string `json:"right"`
	Description string `json:"description"`
}

type adjudicateOutput struct {
	Decisions      []adjudicateDecision      `json:"decisions"`
	Contradictions []adjudicateContradiction `json:"contradictions"`
	Meta           *adjudicateMeta           `json:"meta"`
}

// Adjudicate is pipeline stage V1.2: per-component and per-isolated-point LLM
// adjudication over the s2 ClusterResult. Inputs are a snapshot (no cascade);
// output carries final points (with embeddings) and contradictions for V1.3.
// A component whose decisions omit any member is voided wholesale: its
// historical points are untouched, all its members become new points, and its
// contradictions are dropped. Never persists.
func Adjudicate(ctx context.Context, agentID string, em Embedder, cl ClusterLLM, res ClusterResult, maxConc int) (Adjudication, error) {
	if maxConc <= 0 {
		maxConc = 4
	}
	// Flatten: every component (plain + conflict-queue) is one LLM unit;
	// conflict queue order is preserved.
	units := append([]Component{}, res.Components...)
	for _, queue := range res.Conflicts {
		units = append(units, queue...)
	}

	type unitResult struct {
		idx int
		out adjudicateOutput
		err error
	}
	results := make([]unitResult, len(units))
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup
	for i, comp := range units {
		wg.Add(1)
		go func(i int, comp Component) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var out adjudicateOutput
			err := cl.ChatJSON(ctx, []llm.Message{{Role: "user", Content: buildAdjudicatePrompt(comp)}}, &out)
			results[i] = unitResult{idx: i, out: out, err: err}
		}(i, comp)
	}
	wg.Wait()

	var out Adjudication
	for _, r := range results {
		if r.err != nil {
			// Network/adjudication failure: void the component — historical
			// points untouched, members become new points (never lost).
			out.void(agentID, units[r.idx])
			continue
		}
		out.applyComponent(ctx, agentID, em, units[r.idx], r.out)
	}

	// Isolated points: one metadata-only call each.
	isoResults := make([]unitResult, len(res.Isolated))
	for i, p := range res.Isolated {
		wg.Add(1)
		go func(i int, p Point) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var o adjudicateOutput
			err := cl.ChatJSON(ctx, []llm.Message{{Role: "user", Content: buildIsolatedPrompt(p)}}, &o)
			isoResults[i] = unitResult{idx: i, out: o, err: err}
		}(i, p)
	}
	wg.Wait()
	for _, r := range isoResults {
		p := res.Isolated[r.idx]
		if r.err != nil {
			out.isolated(ctx, agentID, p, nil)
			continue
		}
		out.isolated(ctx, agentID, p, r.out.Meta)
	}
	return out, nil
}

// applyComponent folds one component's verdict into the adjudication.
func (a *Adjudication) applyComponent(ctx context.Context, agentID string, em Embedder, comp Component, out adjudicateOutput) {
	// Completeness: every member must have a decision. Omission voids the
	// whole component.
	decided := make(map[string]bool)
	for _, d := range out.Decisions {
		decided[d.SourceTopic] = true
	}
	for _, m := range comp.Members {
		if !decided[m.Candidate.Topic] {
			a.void(agentID, comp)
			return
		}
	}

	for _, d := range out.Decisions {
		switch d.Action {
		case "merge":
			// 覆盖式合并：a 的信息覆盖进历史点，a 不新建（"其实我更喜欢
			// Rust" → h1 更新为 Rust）。历史点原 id 保留。
			hist := findHist(comp, d.TargetID)
			pt := updatedPoint(agentID, hist, d.Merged, d.TargetID)
			vec, err := em.Embed(ctx, candidateText(mergeCandidateToFork(d.Merged)))
			if err != nil {
				vec = nil
			}
			a.FinalPoints = append(a.FinalPoints, FinalPoint{Point: pt, Vec: vec, Action: "update"})
		case "keep":
			// 相关但不 merge（或纯独立）：a 新建；可同时带动同蔟历史点
			// update（Go 1.18 → 1.19）。
			a.created(ctx, agentID, em, d.Merged)
			for _, u := range d.Updates {
				hist := findHist(comp, u.TargetID)
				upt := updatedPoint(agentID, hist, u.Merged, u.TargetID)
				uv, err := em.Embed(ctx, candidateText(mergeCandidateToFork(u.Merged)))
				if err != nil {
					uv = nil
				}
				a.FinalPoints = append(a.FinalPoints, FinalPoint{Point: upt, Vec: uv, Action: "update"})
			}
		case "archive":
			hist := findHist(comp, d.TargetID)
			if hist != nil {
				a.Archived = append(a.Archived, ArchivedPoint{Pt: hist.Pt})
			}
			a.created(ctx, agentID, em, d.Merged)
		}
	}
	for _, c := range out.Contradictions {
		left := resolveSide(comp, c.Left)
		right := resolveSide(comp, c.Right)
		if left == "" || right == "" || left == right {
			continue
		}
		a.Contradictions = append(a.Contradictions, store.Contradiction{
			AgentID:     agentID,
			LeftID:      left,
			RightID:     right,
			Description: c.Description,
			Status:      "open",
			CreatedAt:   time.Now().UTC(),
		})
	}
}

// void handles an invalid component (LLM failure or member omission): all
// historical points untouched, all members become new points, contradictions
// dropped. Members keep their s1 embedding (name unchanged).
func (a *Adjudication) void(agentID string, comp Component) {
	now := time.Now().UTC()
	for _, m := range comp.Members {
		pt := store.InterestPoint{
			ID:         newID(m.Candidate.Topic),
			AgentID:    agentID,
			Name:       m.Candidate.Topic,
			Summary:    m.Candidate.Reason,
			Keywords:   m.Candidate.Tags,
			Importance: m.Candidate.Confidence,
			Status:     "active",
			Subjective: m.Candidate.Subjective,
			TurnRange:  m.Candidate.TurnRange,
			Reliability: store.Reliability{
				Confidence: m.Candidate.Confidence,
				Status:     "unknown",
			},
			Freshness: store.Freshness{
				Level:     "unknown",
				UpdatedAt: now,
			},
			FirstSeenAt:    now,
			LastSeenAt:     now,
			SeenCount:      1,
			SourceSessions: []string{},
			WikiWorthy:     m.Candidate.WikiWorthy,
		}
		a.FinalPoints = append(a.FinalPoints, FinalPoint{Point: pt, Vec: m.Vec, Action: "create"})
	}
}

// created appends a new final point from a merged candidate.
func (a *Adjudication) created(ctx context.Context, agentID string, em Embedder, m mergeCandidate) {
	now := time.Now().UTC()
	pt := store.InterestPoint{
		ID:         newID(m.Topic),
		AgentID:    agentID,
		Name:       m.Topic,
		Summary:    m.Reason,
		Keywords:   m.Tags,
		Importance: m.Confidence,
		Status:     "active",
		Subjective: m.Subjective,
		TurnRange:  m.TurnRange,
		Reliability: store.Reliability{
			Confidence: m.Confidence,
			Status:     "unknown",
		},
		Freshness: store.Freshness{
			Level:     "unknown",
			UpdatedAt: now,
		},
		FirstSeenAt:    now,
		LastSeenAt:     now,
		SeenCount:      1,
		SourceSessions: []string{},
		WikiWorthy:     m.WikiWorthy,
	}
	var vec []float32
	if em != nil {
		if v, err := em.Embed(ctx, candidateText(mergeCandidateToFork(m))); err == nil {
			vec = v
		}
	}
	a.FinalPoints = append(a.FinalPoints, FinalPoint{Point: pt, Vec: vec, Action: "create"})
}

// updatedPoint builds the updated historical point (original id preserved).
func updatedPoint(agentID string, hist *HistPoint, m mergeCandidate, id string) store.InterestPoint {
	if hist == nil {
		return store.InterestPoint{
			ID: id, AgentID: agentID, Name: m.Topic, Summary: m.Reason,
			Status: "active", Keywords: m.Tags, Importance: m.Confidence,
		}
	}
	pt := hist.Pt
	pt.Name = m.Topic
	pt.Summary = m.Reason
	pt.Keywords = m.Tags
	pt.Importance = m.Confidence
	pt.Status = "active"
	pt.SeenCount++
	pt.LastSeenAt = time.Now().UTC()
	if m.WikiWorthy != nil {
		pt.WikiWorthy = m.WikiWorthy
	}
	return pt
}

// isolated appends an isolated point (metadata from the LLM when available).
// The s1 embedding is reused (name unchanged); metadata only adjusts flags.
func (a *Adjudication) isolated(ctx context.Context, agentID string, p Point, meta *adjudicateMeta) {
	now := time.Now().UTC()
	m := mergeCandidateFromPoint(p)
	if meta != nil {
		m.Subjective = meta.Subjective
		m.WikiWorthy = meta.WikiWorthy
	}
	pt := store.InterestPoint{
		ID:         newID(m.Topic),
		AgentID:    agentID,
		Name:       m.Topic,
		Summary:    m.Reason,
		Keywords:   m.Tags,
		Importance: m.Confidence,
		Status:     "active",
		Subjective: m.Subjective,
		TurnRange:  m.TurnRange,
		Reliability: store.Reliability{
			Confidence: m.Confidence,
			Status:     "unknown",
		},
		Freshness: store.Freshness{
			Level:     "unknown",
			UpdatedAt: now,
		},
		FirstSeenAt:    now,
		LastSeenAt:     now,
		SeenCount:      1,
		SourceSessions: []string{},
		WikiWorthy:     m.WikiWorthy,
	}
	if meta != nil {
		pt.Reliability.Confidence = meta.Confidence
		pt.Reliability.Status = meta.ReliabilityStatus
		pt.Freshness.Level = meta.FreshnessLevel
		pt.Freshness.TTLDays = meta.TTLDays
	}
	a.FinalPoints = append(a.FinalPoints, FinalPoint{Point: pt, Vec: p.Vec, Action: "create"})
}

func findHist(comp Component, id string) *HistPoint {
	for i := range comp.Hist {
		if comp.Hist[i].Pt.ID == id {
			return &comp.Hist[i]
		}
	}
	return nil
}

func resolveSide(comp Component, s string) string {
	// Historical ids pass through; member topics resolve to their new ids.
	for _, h := range comp.Hist {
		if h.Pt.ID == s {
			return s
		}
	}
	for _, m := range comp.Members {
		if m.Candidate.Topic == s {
			return newID(s)
		}
	}
	return ""
}

func mergeCandidateFromPoint(p Point) mergeCandidate {
	return mergeCandidate{
		Topic:      p.Candidate.Topic,
		Reason:     p.Candidate.Reason,
		Confidence: p.Candidate.Confidence,
		Tags:       p.Candidate.Tags,
		TurnRange:  p.Candidate.TurnRange,
		Subjective: p.Candidate.Subjective,
		WikiWorthy: p.Candidate.WikiWorthy,
	}
}

// buildAdjudicatePrompt renders the component's current+historical points and
// asks the LLM for a per-member verdict (merge/update/keep/archive) plus any
// contradictions. Members are referred to by topic, historical points by id.
func buildAdjudicatePrompt(comp Component) string {
	var b strings.Builder
	b.WriteString("You are adjudicating interest points for a personal memory system. For each NEW point, decide how it relates to the similar HISTORICAL point(s) in its group.\n\n")
	b.WriteString("NEW points (refer to them by exact topic):\n")
	for i, m := range comp.Members {
		b.WriteString(fmt.Sprintf("  %d. topic: %q\n     reason: %s\n     confidence: %.2f\n",
			i+1, m.Candidate.Topic, m.Candidate.Reason, m.Candidate.Confidence))
	}
	b.WriteString("\nHISTORICAL points (refer to them by id):\n")
	for i, h := range comp.Hist {
		b.WriteString(fmt.Sprintf("  %d. id: %s\n     name: %s\n     summary: %s\n",
			i+1, h.Pt.ID, h.Pt.Name, h.Pt.Summary))
	}
	b.WriteString(`
For EVERY new point output exactly one decision. Actions:
- "merge": the new point OVERTAKES the historical point (covering merge, e.g. "actually I prefer Rust now" → the historical point is updated to the new preference). The historical point keeps its id; the new point is not created separately.
- "keep": the new point is related but not mergeable (or fully independent) → create it as a new point; the historical point is untouched UNLESS listed in "updates".
- "archive": the new point overturns the historical point → archive the historical point, create the new point.

"updates" (optional): historical points you decide to update as a side-effect of this decision (e.g. the new point says "Go 1.19" and a historical point said "Go 1.18" → update it). Each entry: target_id (historical id) + merged (its post-update form).

The "merged" object is the FINAL interest point: give its name, summary, confidence, tags, subjective (bool), and reliability/freshness metadata (reliability_status: supported|contested|unknown, confidence 0-1, freshness_level: fresh|aging|stale|unknown, ttl_days, wiki_worthy). You do NOT assign ids — the system derives them.

Also list any contradictions between a new point and a historical point: "left"/"right" use the new point's exact topic or the historical point's id.

Return ONLY valid JSON:
{
  "decisions": [
    { "source_topic": "<new point topic>", "action": "merge"|"keep"|"archive", "target_id": "<historical id or empty>",
      "merged": { "name": "...", "summary": "...", "confidence": 0.0-1.0, "tags": [...], "subjective": false,
                  "reliability_status": "...", "freshness_level": "...", "ttl_days": 0, "wiki_worthy": true },
      "updates": [ { "target_id": "<historical id>", "merged": { "name": "...", "summary": "...", "confidence": 0.0-1.0, "tags": [...] } } ]
    }
  ],
  "contradictions": [
    { "left": "<topic or id>", "right": "<topic or id>", "description": "..." }
  ]
}`)
	return b.String()
}

// buildIsolatedPrompt asks for metadata only (no merge/contradiction task).
func buildIsolatedPrompt(p Point) string {
	var b strings.Builder
	b.WriteString("You are assigning metadata to a single interest point for a personal memory system. Return ONLY valid JSON:\n\n")
	b.WriteString(fmt.Sprintf("topic: %q\nreason: %s\nconfidence: %.2f\n",
		p.Candidate.Topic, p.Candidate.Reason, p.Candidate.Confidence))
	b.WriteString(`
{
  "meta": {
    "subjective": true|false,
    "reliability_status": "supported"|"contested"|"unknown",
    "confidence": 0.0-1.0,
    "freshness_level": "fresh"|"aging"|"stale"|"unknown",
    "ttl_days": 0-365,
    "wiki_worthy": true|false
  }
}`)
	return b.String()
}
