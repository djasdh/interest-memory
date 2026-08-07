package verify

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/store"
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
		prompt := buildClaimsPrompt(p, s.cfg.Language)
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

func buildClaimsPrompt(p store.InterestPoint, lang string) string {
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
	b.WriteString(fmt.Sprintf("\n\nWrite all claim text and evidence in '%s'.\n", lang))
	return b.String()
}

// contradictionPair is one LLM-detected contradiction among claims.
// IsContradiction is a pointer so an absent field (legacy model) defaults to
// true and keeps the current behaviour instead of silently zeroing recall.
type contradictionPair struct {
	LeftText        string  `json:"left_text"`
	RightText       string  `json:"right_text"`
	Description     string  `json:"description"`
	IsContradiction *bool   `json:"is_contradiction"`
	Confidence      float64 `json:"confidence"`
}

type contradictionExtract struct {
	Pairs []contradictionPair `json:"contradictions"`
}

// FlagContradictions asks the LLM to spot contradictions between claims
// (verify#2). Claims are grouped in windows to bound prompt size. When an
// embedder is available, claims are semantically grouped first: only
// same-topic candidate pairs (cosine >= SimThreshold) are submitted to the
// LLM, so it judges instead of searching an O(n²) space. Degraded to the full
// prompt when embedding fails.
func (s *service) FlagContradictions(ctx context.Context, agentID string, claims []store.Claim) ([]store.Contradiction, error) {
	var out []store.Contradiction
	if len(claims) == 0 {
		return out, nil
	}
	const window = 20
	for i := 0; i < len(claims); i += window {
		end := min(i+window, len(claims))
		group := claims[i:end]
		var (
			prompt  string
			allowed map[string]bool // semantic-grouping mode: pairs the LLM may return
		)
		if s.embed != nil {
			cands, err := s.semanticCandidates(ctx, group)
			switch {
			case err != nil:
				prompt = s.buildContradictionPrompt(group) // degraded: full scan
			case len(cands) == 0:
				continue // no same-topic pairs, nothing to ask the LLM
			default:
				prompt = s.buildCandidatePrompt(group, cands)
				allowed = buildAllowedSet(group, cands)
			}
		} else {
			prompt = s.buildContradictionPrompt(group)
		}
		var ex contradictionExtract
		if err := s.llm.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &ex); err != nil {
			continue // degraded: no contradictions flagged this window
		}
		for _, p := range ex.Pairs {
			if p.IsContradiction != nil && !*p.IsContradiction {
				continue
			}
			if s.cfg.MinConfidence > 0 && p.Confidence < s.cfg.MinConfidence {
				continue
			}
			if IsNonContradiction(p.Description) {
				continue
			}
			left := findClaim(group, p.LeftText)
			right := findClaim(group, p.RightText)
			if left == nil || right == nil || left.ID == right.ID {
				continue
			}
			// Canonicalize the pair ordering so the same two claims always
			// yield the same id (upsert dedups reversed pairs).
			l, r := left.ID, right.ID
			if l > r {
				l, r = r, l
			}
			// Semantic-grouping mode: reject pairs outside the pre-filtered
			// candidate set (precision over recall).
			if allowed != nil && !allowed[l+"|"+r] {
				continue
			}
			out = append(out, store.Contradiction{
				ID:          newID(agentID, l, r),
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

// candidatePair is one same-topic claim pair (indices into the window group)
// ranked by embedding similarity.
type candidatePair struct {
	i, j int
	sim  float64
}

// semanticCandidates embeds the window's claims and returns the same-topic
// pairs with cosine >= SimThreshold, sorted by similarity descending and
// capped at MaxCandidates. An error degrades the caller to the full prompt.
func (s *service) semanticCandidates(ctx context.Context, group []store.Claim) ([]candidatePair, error) {
	texts := make([]string, len(group))
	for i := range group {
		texts[i] = group[i].Text
	}
	vecs, err := s.embed.EmbedBatch(ctx, texts)
	if err != nil {
		return nil, err
	}
	var cands []candidatePair
	for i := 0; i < len(vecs); i++ {
		for j := i + 1; j < len(vecs); j++ {
			sim := cosine(vecs[i], vecs[j])
			if sim >= s.cfg.SimThreshold {
				cands = append(cands, candidatePair{i: i, j: j, sim: sim})
			}
		}
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].sim > cands[b].sim })
	if len(cands) > s.cfg.MaxCandidates {
		cands = cands[:s.cfg.MaxCandidates]
	}
	return cands, nil
}

// buildAllowedSet maps the candidate pairs to their canonical (sorted)
// claim-id keys, so FlagContradictions can reject LLM returns outside it.
func buildAllowedSet(group []store.Claim, cands []candidatePair) map[string]bool {
	set := make(map[string]bool, len(cands))
	for _, c := range cands {
		l, r := group[c.i].ID, group[c.j].ID
		if l > r {
			l, r = r, l
		}
		set[l+"|"+r] = true
	}
	return set
}

// cosine returns the cosine similarity of two vectors (0 for degenerate ones).
func cosine(a, b []float32) float64 {
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

func (s *service) buildContradictionPrompt(claims []store.Claim) string {
	var b strings.Builder
	lang := s.cfg.Language
	if lang == "" {
		lang = "English"
	}
	b.WriteString("Below are claims from a memory wiki. Identify candidate pairs that may contradict each other.\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Put ALL suspicious pairs into the contradictions array; each entry must carry is_contradiction (true/false) and confidence (0~1).\n")
	b.WriteString("- Claims on unrelated topics are NOT contradictions (e.g. 'PostgreSQL deployment' vs 'Python syntax'); do not include such pairs.\n")
	b.WriteString("- Counter-example: 'MySQL is faster' vs 'SQLite is faster' both say 'faster' but differ in subject; not a contradiction, do not include.\n")
	b.WriteString(fmt.Sprintf("- Write description in '%s'.\n", lang))
	b.WriteString("Return ONLY valid JSON.\n\n")
	for i, c := range claims {
		b.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, c.ID, c.Text))
	}
	b.WriteString(`
{
  "contradictions": [
    {
      "left_text": "exact text of claim A",
      "right_text": "exact text of claim B",
      "description": "why they may contradict",
      "is_contradiction": true,
      "confidence": 0.0-1.0
    }
  ]
}`)
	return b.String()
}

// buildCandidatePrompt renders the semantic-grouping prompt: the pre-filtered
// same-topic candidate pairs with their full texts, asking the LLM to judge
// only those pairs. left_text/right_text must be copied verbatim so findClaim
// can resolve them back to claim ids.
func (s *service) buildCandidatePrompt(group []store.Claim, cands []candidatePair) string {
	var b strings.Builder
	lang := s.cfg.Language
	if lang == "" {
		lang = "English"
	}
	b.WriteString("Below are pre-filtered candidate pairs from a memory wiki (each pair may be about the same topic). Judge which pairs are true contradictions.\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Judge ONLY the pairs listed below; do not add or modify any pair.\n")
	b.WriteString("- Each entry must carry is_contradiction (true/false) and confidence (0~1).\n")
	b.WriteString("- If two claims are actually unrelated or not contradictory, set is_contradiction=false.\n")
	b.WriteString("- left_text/right_text must be copied verbatim from the quoted texts below.\n")
	b.WriteString(fmt.Sprintf("- Write description in '%s'.\n", lang))
	b.WriteString("Return ONLY valid JSON.\n\n")
	for k, c := range cands {
		b.WriteString(fmt.Sprintf("%d. \"%s\" ↔ \"%s\"\n", k+1, group[c.i].Text, group[c.j].Text))
	}
	b.WriteString(`
{
  "contradictions": [
    {
      "left_text": "verbatim text of claim A",
      "right_text": "verbatim text of claim B",
      "description": "why they contradict",
      "is_contradiction": true,
      "confidence": 0.0-1.0
    }
  ]
}`)
	return b.String()
}

// Negation-aware filters for contradiction descriptions. A description that
// denies a contradiction (e.g. "not contradictory", "一致") is treated as a
// non-contradiction even when the LLM still marks the pair true.
var (
	// strongNegRe: explicit denials — matched, the pair is not a contradiction.
	strongNegRe = regexp.MustCompile(`(?i)not contradictory|not a contradiction|no contradiction|不矛盾|没有矛盾|无矛盾|无关|互补`)
	// weakNegRe: agreement wording that denies a contradiction UNLESS itself
	// negated (一致 in 不一致, consistent in inconsistent). Kept to words that
	// almost never appear in a genuine contradiction description.
	weakNegRe = regexp.MustCompile(`(?i)consistent|一致|同义`)
	// negatedRe: negation forms of the weak words — these describe an actual
	// conflict and must NOT be dropped.
	negatedRe = regexp.MustCompile(`(?i)inconsistent|not consistent|不\s*一致|非\s*一致|不\s*同义|非\s*同义`)
)

// IsNonContradiction reports whether a description signals the pair is NOT a
// contradiction: a strong denial phrase, or agreement wording that is not
// itself negated. Negated forms (不一致 / inconsistent) describe conflict and
// are left through.
func IsNonContradiction(desc string) bool {
	if strongNegRe.MatchString(desc) {
		return true
	}
	if negatedRe.MatchString(desc) {
		return false
	}
	return weakNegRe.MatchString(desc)
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
	// Fuzzy: allow a prefix match when the LLM truncated the text, but only
	// when it is unambiguous — multiple prefix hits mean we cannot tell which
	// claim was meant, and a wrong edge is worse than a missed one.
	var hit *store.Claim
	n := 0
	for i := range claims {
		if strings.HasPrefix(claims[i].Text, needle) || strings.HasPrefix(needle, claims[i].Text) {
			hit = &claims[i]
			n++
		}
	}
	if n == 1 {
		return hit
	}
	return nil
}
