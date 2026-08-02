package verify

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"interest-memory/internal/fork"
	"interest-memory/internal/llm"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/websearch"
)

// verifyResult is the LLM's JSON verdict for a single candidate.
type verifyResult struct {
	IsSubjective   bool     `json:"is_subjective"`
	Supported      bool     `json:"supported"`
	Confidence     float64  `json:"confidence"`
	Status         string   `json:"status"` // supported | contested | unknown
	Evidence       []string `json:"evidence"`
	FreshLevel     string   `json:"freshness_level"` // fresh | aging | stale | unknown
	TTLDays        int      `json:"ttl_days"`
	SearchQuery    string   `json:"search_query,omitempty"`
	Relation       string   `json:"relation"`        // none | supersede | update | delete
	RelationReason string   `json:"relation_reason"` // why this relation holds
}

// VerifyCandidates fact-checks each candidate (verify#1), concurrently with
// bounded parallelism. For each candidate it recalls the most similar
// historical interest point (when vec/embed are wired), gathers web evidence
// (skipped for subjective candidates or when web search is disabled), then
// asks the LLM to produce a JSON verdict including subjectivity and the
// relation to that historical point. Candidates whose verdict is
// contested/unknown still pass through — the status drives later grading,
// not a hard reject.
func (s *service) VerifyCandidates(ctx context.Context, agentID string, cands []fork.Candidate) ([]Verified, error) {
	out := make([]Verified, len(cands))
	sem := make(chan struct{}, s.cfg.MaxConcurrency)
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c fork.Candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := s.verifyOne(ctx, agentID, c)
			if err != nil {
				// A fact-check failure must not block the pipeline: fall back
				// to an unknown-status verdict with the candidate's own data.
				v = degradedVerifier(c)
			}
			out[i] = v
		}(i, c)
	}
	wg.Wait()
	return out, nil
}

func (s *service) verifyOne(ctx context.Context, agentID string, c fork.Candidate) (Verified, error) {
	hist := s.findSimilar(ctx, agentID, c)
	evidence, query := s.gatherEvidence(ctx, c)
	prompt := buildVerifyPrompt(c, evidence, hist)
	var vr verifyResult
	if err := s.llm.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &vr); err != nil {
		return Verified{}, fmt.Errorf("verify: candidate %q: %w", c.Topic, err)
	}
	vr.normalize()

	subjective := c.Subjective || vr.IsSubjective
	relation := normalizeRelation(vr.Relation, hist)

	var ev []store.Evidence
	for _, e := range evidence {
		ev = append(ev, store.Evidence{
			Kind:       "web",
			SourceID:   e.URL,
			URL:        e.URL,
			Query:      query,
			CapturedAt: now(),
			Excerpt:    truncate(e.Snippet, 300),
		})
	}
	if len(ev) == 0 {
		// No web evidence: provenance is the conversation itself, located to
		// the turn range the candidate was extracted from.
		ev = append(ev, store.Evidence{
			Kind:      "session",
			SourceID:  "",
			TurnRange: c.TurnRange,
			Excerpt:   truncate(c.Reason, 300),
		})
	}

	return Verified{
		Candidate:      c,
		Subjective:     subjective,
		Relation:       relation,
		RelationToID:   relationToID(relation, hist),
		RelationReason: vr.RelationReason,
		Reliability: store.Reliability{
			Confidence: vr.Confidence,
			Status:     vr.Status,
			Evidence:   ev,
		},
		Freshness: store.Freshness{
			Level:     vr.FreshLevel,
			UpdatedAt: now(),
			TTLDays:   vr.TTLDays,
		},
	}, nil
}

// findSimilar recalls the most similar historical interest point for the
// candidate. Returns nil when vec/embed are unavailable, retrieval fails, or
// no interest-point hit clears the similarity bar (0.6).
func (s *service) findSimilar(ctx context.Context, agentID string, c fork.Candidate) *store.InterestPoint {
	if s.embed == nil || s.vec == nil {
		return nil
	}
	text := c.Topic
	if c.Reason != "" {
		text += "\n" + c.Reason
	}
	q, err := s.embed.Embed(ctx, text)
	if err != nil {
		return nil
	}
	hits, err := s.vec.Search(ctx, agentID, q, 10)
	if err != nil {
		return nil
	}
	var best *vec.Hit
	for i := range hits {
		if hits[i].Kind != "interest_point" {
			continue
		}
		if best == nil || hits[i].Score > best.Score {
			best = &hits[i]
		}
	}
	if best == nil || float64(best.Score) < 0.6 {
		return nil
	}
	p, err := s.store.GetInterestPoint(ctx, agentID, best.ID)
	if err != nil || p == nil {
		return nil
	}
	return p
}

// gatherEvidence runs a web search for the candidate when enabled and not
// subjective, returning the top items and the query used. On any search error
// it returns nil (degraded). Subjective candidates skip web fact-checking but
// the LLM verification still runs (关系判断照常).
func (s *service) gatherEvidence(ctx context.Context, c fork.Candidate) ([]websearch.SearchItem, string) {
	if s.search == nil || !s.cfg.UseWebSearch || c.Subjective {
		return nil, ""
	}
	query := c.Topic
	if len(c.Tags) > 0 {
		query = c.Topic + " " + strings.Join(c.Tags[:min(3, len(c.Tags))], " ")
	}
	items, err := s.search.Search(ctx, query, s.cfg.SearchMax)
	if err != nil || len(items) == 0 {
		return nil, ""
	}
	return items, query
}

func buildVerifyPrompt(c fork.Candidate, evidence []websearch.SearchItem, hist *store.InterestPoint) string {
	var b strings.Builder
	b.WriteString("You are a fact-checker for a personal memory system. Judge whether the following interest point extracted from a conversation is reliable, how fresh it is, and how it relates to the most similar historical memory (if any).\n\n")
	b.WriteString(fmt.Sprintf("Topic: %s\n", c.Topic))
	b.WriteString(fmt.Sprintf("Claim/reason from conversation: %s\n", c.Reason))
	b.WriteString(fmt.Sprintf("Extraction confidence: %.2f\n", c.Confidence))
	if len(c.Tags) > 0 {
		b.WriteString(fmt.Sprintf("Tags: %s\n", strings.Join(c.Tags, ", ")))
	}
	if hist != nil {
		b.WriteString("\nMost similar historical memory (may supersede this claim, or this claim may supersede it):\n")
		b.WriteString(fmt.Sprintf("- ID: %s\n- Name: %s\n- Summary: %s\n",
			hist.ID, hist.Name, truncate(hist.Summary, 300)))
		b.WriteString(fmt.Sprintf("- Reliability: status=%s confidence=%.2f\n", hist.Reliability.Status, hist.Reliability.Confidence))
	}
	if len(evidence) > 0 {
		b.WriteString("\nWeb search evidence (may be empty if unreliable):\n")
		for i, e := range evidence {
			b.WriteString(fmt.Sprintf("%d. %s — %s (%s)\n", i+1, e.Title, truncate(e.Snippet, 200), e.URL))
		}
	} else {
		b.WriteString("\n(No web evidence available — rely on the claim wording and general knowledge.)\n")
	}
	b.WriteString(`
Return ONLY valid JSON, no other text, with this shape:
{
  "is_subjective": true | false (is this the user's own preference/opinion rather than an objective fact?),
  "supported": true,
  "confidence": 0.0-1.0,
  "status": "supported" | "contested" | "unknown",
  "evidence": ["short reason 1", "short reason 2"],
  "freshness_level": "fresh" | "aging" | "stale" | "unknown",
  "ttl_days": 0-365,
  "search_query": "the query that should be used if future verification is needed",
  "relation": "none" | "supersede" | "update" | "delete",
  "relation_reason": "one sentence explaining the chosen relation (omit if relation=none)"
}

Relation semantics (only meaningful when a similar historical memory exists):
- "supersede": the new claim replaces the historical one (the user moved on)
- "update": the new claim corrects or refines the historical one (merge into it)
- "delete": the new claim shows the historical one is no longer true (remove it)
- "none": no meaningful relation to the historical memory`)
	return b.String()
}

func (v *verifyResult) normalize() {
	switch v.Status {
	case "supported", "contested", "unknown":
	default:
		if v.Supported {
			v.Status = "supported"
		} else if v.Confidence < 0.4 {
			v.Status = "contested"
		} else {
			v.Status = "unknown"
		}
	}
	if v.Confidence <= 0 || v.Confidence > 1 {
		v.Confidence = 0.5
	}
	switch v.FreshLevel {
	case "fresh", "aging", "stale", "unknown":
	default:
		v.FreshLevel = "unknown"
	}
	if v.TTLDays < 0 {
		v.TTLDays = 0
	}
}

// normalizeRelation maps the LLM's relation string to a Relation constant.
// Without a historical point the relation is always none.
func normalizeRelation(s string, hist *store.InterestPoint) Relation {
	if hist == nil {
		return RelationNone
	}
	switch Relation(strings.ToLower(strings.TrimSpace(s))) {
	case RelationSupersede:
		return RelationSupersede
	case RelationUpdate:
		return RelationUpdate
	case RelationDelete:
		return RelationDelete
	default:
		return RelationNone
	}
}

func relationToID(r Relation, hist *store.InterestPoint) string {
	if r != RelationNone && hist != nil {
		return hist.ID
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
