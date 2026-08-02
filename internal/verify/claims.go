package verify

import (
	"context"
	"fmt"
	"strings"

	"interest-memory/internal/llm"
	"interest-memory/internal/store"
)

// claimExtract is the LLM's JSON output for CheckClaims (one page's claims).
type claimExtract struct {
	Claims []struct {
		Text       string   `json:"text"`
		Confidence float64  `json:"confidence"`
		Status     string   `json:"status"` // supported | contested | stale
		Evidence   []string `json:"evidence"`
	} `json:"claims"`
}

// CheckClaims extracts structured claims from each interest point (verify#2).
// A page without a page id yet is keyed by the interest point id; the caller
// later re-points claims at the compiled page via page_id.
func (s *service) CheckClaims(ctx context.Context, agentID string, pts []store.InterestPoint) ([]store.Claim, error) {
	var out []store.Claim
	for _, p := range pts {
		prompt := buildClaimsPrompt(p)
		var ex claimExtract
		if err := s.llm.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &ex); err != nil {
			// Degraded: derive a single claim from the summary.
			out = append(out, store.Claim{
				ID:         newID(agentID, p.ID, "summary"),
				AgentID:    agentID,
				PageID:     p.ID,
				Text:       p.Summary,
				Status:     p.Reliability.Status,
				Confidence: p.Reliability.Confidence,
				Freshness:  p.Freshness,
			})
			continue
		}
		if len(ex.Claims) == 0 {
			continue
		}
		for _, c := range ex.Claims {
			status := c.Status
			switch status {
			case "supported", "contested", "stale":
			default:
				status = p.Reliability.Status
			}
			conf := c.Confidence
			if conf <= 0 || conf > 1 {
				conf = p.Reliability.Confidence
			}
			var ev []store.Evidence
			for _, e := range c.Evidence {
				ev = append(ev, store.Evidence{Kind: "session", SourceID: p.ID, Excerpt: truncate(e, 300)})
			}
			out = append(out, store.Claim{
				ID:         newID(agentID, p.ID, c.Text),
				AgentID:    agentID,
				PageID:     p.ID,
				Text:       c.Text,
				Status:     status,
				Confidence: conf,
				Evidence:   ev,
				Freshness:  p.Freshness,
			})
		}
	}
	return out, nil
}

func buildClaimsPrompt(p store.InterestPoint) string {
	var b strings.Builder
	b.WriteString("Extract the factual claims implied by this interest point. Return ONLY valid JSON.\n\n")
	b.WriteString(fmt.Sprintf("Topic: %s\nSummary: %s\nReliability: confidence=%.2f status=%s\n",
		p.Name, p.Summary, p.Reliability.Confidence, p.Reliability.Status))
	b.WriteString(`
{
  "claims": [
    {
      "text": "one factual assertion",
      "confidence": 0.0-1.0,
      "status": "supported" | "contested" | "stale",
      "evidence": ["why this is believed"]
    }
  ]
}`)
	return b.String()
}

// contradictionPair is one LLM-detected contradiction among claims.
type contradictionPair struct {
	LeftText    string `json:"left_text"`
	RightText   string `json:"right_text"`
	Description string `json:"description"`
}

type contradictionExtract struct {
	Pairs []contradictionPair `json:"contradictions"`
}

// FlagContradictions asks the LLM to spot contradictions between claims
// (verify#2). Claims are grouped in windows to bound prompt size.
func (s *service) FlagContradictions(ctx context.Context, agentID string, claims []store.Claim) ([]store.Contradiction, error) {
	var out []store.Contradiction
	if len(claims) == 0 {
		return out, nil
	}
	const window = 20
	for i := 0; i < len(claims); i += window {
		end := min(i+window, len(claims))
		group := claims[i:end]
		prompt := buildContradictionPrompt(group)
		var ex contradictionExtract
		if err := s.llm.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &ex); err != nil {
			continue // degraded: no contradictions flagged this window
		}
		for _, p := range ex.Pairs {
			left := findClaim(group, p.LeftText)
			right := findClaim(group, p.RightText)
			if left == nil || right == nil || left.ID == right.ID {
				continue
			}
			out = append(out, store.Contradiction{
				ID:          newID(agentID, left.ID, right.ID),
				AgentID:     agentID,
				LeftID:      left.ID,
				RightID:     right.ID,
				Description: truncate(p.Description, 500),
				Status:      "open",
				CreatedAt:   now(),
			})
		}
	}
	return out, nil
}

func buildContradictionPrompt(claims []store.Claim) string {
	var b strings.Builder
	b.WriteString("Below are claims from a memory wiki. Identify pairs that directly contradict each other. Return ONLY valid JSON.\n\n")
	for i, c := range claims {
		b.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, c.ID, c.Text))
	}
	b.WriteString(`
{
  "contradictions": [
    {
      "left_text": "exact text of claim A",
      "right_text": "exact text of claim B",
      "description": "why they contradict"
    }
  ]
}`)
	return b.String()
}

func findClaim(claims []store.Claim, text string) *store.Claim {
	needle := strings.TrimSpace(text)
	if needle == "" {
		return nil
	}
	for i := range claims {
		if strings.TrimSpace(claims[i].Text) == needle {
			return &claims[i]
		}
	}
	// Fuzzy: allow a prefix match when the LLM truncated the text.
	for i := range claims {
		if strings.HasPrefix(claims[i].Text, needle) || strings.HasPrefix(needle, claims[i].Text) {
			return &claims[i]
		}
	}
	return nil
}
