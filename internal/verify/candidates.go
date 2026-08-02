package verify

import (
	"context"
	"fmt"
	"strings"

	"interest-memory/internal/fork"
	"interest-memory/internal/llm"
	"interest-memory/internal/store"
)

// verifyResult is the LLM's JSON verdict for a single candidate.
type verifyResult struct {
	Supported   bool     `json:"supported"`
	Confidence  float64  `json:"confidence"`
	Status      string   `json:"status"` // supported | contested | unknown
	Evidence    []string `json:"evidence"`
	FreshLevel  string   `json:"freshness_level"` // fresh | aging | stale | unknown
	TTLDays     int      `json:"ttl_days"`
	SearchQuery string   `json:"search_query,omitempty"`
}

// VerifyCandidates fact-checks each candidate (verify#1). For each candidate
// it gathers web evidence (when enabled) then asks the LLM to produce a JSON
// verdict. Candidates whose verdict is contested/unknown still pass through —
// the status drives later grading, not a hard reject.
func (s *service) VerifyCandidates(ctx context.Context, agentID string, cands []fork.Candidate) ([]Verified, error) {
	out := make([]Verified, 0, len(cands))
	for _, c := range cands {
		v, err := s.verifyOne(ctx, c)
		if err != nil {
			// A fact-check failure must not block the pipeline: fall back to
			// an unknown-status verdict with the candidate's own confidence.
			v = Verified{
				Candidate: c,
				Reliability: store.Reliability{
					Confidence: c.Confidence,
					Status:     "unknown",
					Evidence:   nil,
				},
				Freshness: store.Freshness{Level: "unknown", UpdatedAt: now(), TTLDays: 0},
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *service) verifyOne(ctx context.Context, c fork.Candidate) (Verified, error) {
	evidence := s.gatherEvidence(ctx, c)
	prompt := buildVerifyPrompt(c, evidence)
	var vr verifyResult
	if err := s.llm.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &vr); err != nil {
		return Verified{}, fmt.Errorf("verify: candidate %q: %w", c.Topic, err)
	}
	vr.normalize()

	var ev []store.Evidence
	for _, e := range evidence {
		ev = append(ev, store.Evidence{Kind: "web", SourceID: e.URL, Excerpt: truncate(e.Snippet, 300)})
	}
	if len(ev) == 0 {
		// No web evidence: provenance is the conversation itself.
		ev = append(ev, store.Evidence{Kind: "session", SourceID: "", Excerpt: truncate(c.Reason, 300)})
	}

	return Verified{
		Candidate: c,
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

// gatherEvidence runs a web search for the candidate when enabled and returns
// the top items. On any search error it returns nil (degraded).
func (s *service) gatherEvidence(ctx context.Context, c fork.Candidate) []SearchItem {
	if s.search == nil || !s.cfg.UseWebSearch {
		return nil
	}
	query := c.Topic
	if len(c.Tags) > 0 {
		query = c.Topic + " " + strings.Join(c.Tags[:min(3, len(c.Tags))], " ")
	}
	items, err := s.search.Search(ctx, query, s.cfg.SearchMax)
	if err != nil || len(items) == 0 {
		return nil
	}
	return items
}

func buildVerifyPrompt(c fork.Candidate, evidence []SearchItem) string {
	var b strings.Builder
	b.WriteString("You are a fact-checker for a personal memory system. Judge whether the following interest point extracted from a conversation is reliable, and how fresh it is.\n\n")
	b.WriteString(fmt.Sprintf("Topic: %s\n", c.Topic))
	b.WriteString(fmt.Sprintf("Claim/reason from conversation: %s\n", c.Reason))
	b.WriteString(fmt.Sprintf("Extraction confidence: %.2f\n", c.Confidence))
	if len(c.Tags) > 0 {
		b.WriteString(fmt.Sprintf("Tags: %s\n", strings.Join(c.Tags, ", ")))
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
  "supported": true,
  "confidence": 0.0-1.0,
  "status": "supported" | "contested" | "unknown",
  "evidence": ["short reason 1", "short reason 2"],
  "freshness_level": "fresh" | "aging" | "stale" | "unknown",
  "ttl_days": 0-365,
  "search_query": "the query that should be used if future verification is needed"
}`)
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
