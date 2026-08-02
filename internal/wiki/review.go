package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"interest-memory/internal/llm"

	"my-agent-core/types"
)

// reviewSuggestion is one item in the review tool's output.
type reviewSuggestion struct {
	Type    string `json:"type"`    // contradiction | duplicate | stale_reference | improvement
	PageID  string `json:"page_id"` // related existing page ("" when none)
	Message string `json:"message"`
}

// reviewResult is the LLM's JSON output for a draft review.
type reviewResult struct {
	Summary     string             `json:"summary"`
	Suggestions []reviewSuggestion `json:"suggestions"`
}

// NewReviewTool returns the review tool: the agent MUST call it before the
// formal write. It is strictly read-only — it semantic-searches existing wiki
// pages relevant to the draft, then asks the LLM to spot contradictions /
// duplicates / stale references and propose edits. It returns a suggestion
// list; the main agent decides what to adopt.
func NewReviewTool(deps ToolsDeps, agentID string) types.Tool {
	return types.Tool{
		Name:        "review",
		Description: "Read-only review of a draft BEFORE writing it to the wiki. Provide the draft content you plan to write; this checks it against existing wiki pages and returns suggestions (contradictions, duplicates, stale references, improvements). Call this before every wiki_write. It never modifies anything.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"draft": map[string]any{
					"type":        "string",
					"description": "The draft content (markdown) you are about to write",
				},
				"page_id": map[string]any{
					"type":        "string",
					"description": "Optional target page id being updated (for context)",
				},
			},
			"required": []string{"draft"},
		},
		Execute: func(_ types.Context, args types.ArgsMap, _ <-chan struct{}) (string, error) {
			draft, _ := args["draft"].(string)
			pageID, _ := args["page_id"].(string)
			if strings.TrimSpace(draft) == "" {
				return "", fmt.Errorf("review: missing 'draft'")
			}
			return reviewDraft(context.Background(), deps, agentID, draft, pageID)
		},
	}
}

// reviewDraft collects relevant existing pages (read-only) and asks the LLM
// for a structured review. Degrades to an empty-suggestion JSON on any error.
func reviewDraft(ctx context.Context, deps ToolsDeps, agentID, draft, pageID string) (string, error) {
	relevant := relatedPages(ctx, deps, agentID, draft, pageID)

	if deps.LLM == nil {
		out, _ := json.Marshal(reviewResult{
			Summary: "review: no LLM configured — draft not reviewed",
		})
		return string(out) + "\n", nil
	}

	var b strings.Builder
	b.WriteString("You are a wiki reviewer. Review the draft below against existing wiki pages and return suggestions the writer should consider. Only point out real issues: contradictions, near-duplicates, stale references to superseded pages, and concrete improvements. Be concise.\n\n")
	b.WriteString("## Draft to write\n\n")
	b.WriteString(draft)
	if pageID != "" {
		b.WriteString(fmt.Sprintf("\n\n(Target page id: %s — an update to an existing page.)\n", pageID))
	}
	if len(relevant) > 0 {
		b.WriteString("\n## Relevant existing wiki pages\n\n")
		for i, r := range relevant {
			b.WriteString(fmt.Sprintf("%d. [%s] %s (status=%s)\n   %s\n", i+1, r.ID, r.Title, r.Status, truncate(r.BodyMD, 400)))
		}
	} else {
		b.WriteString("\n(No closely related existing pages found.)\n")
	}
	b.WriteString(`
Return ONLY valid JSON:
{
  "summary": "one sentence overall assessment",
  "suggestions": [
    {"type": "contradiction" | "duplicate" | "stale_reference" | "improvement",
     "page_id": "related page id or empty",
     "message": "what to change and why"}
  ]
}`)

	var vr reviewResult
	if err := deps.LLM.ChatJSON(ctx, []llm.Message{{Role: "user", Content: b.String()}}, &vr); err != nil {
		out, _ := json.Marshal(reviewResult{Summary: "review: verdict failed — draft not reviewed"})
		return string(out) + "\n", nil
	}
	out, _ := json.Marshal(vr)
	return string(out) + "\n", nil
}

// relatedPages semantically searches existing wiki pages related to the
// draft (read-only). Falls back to keyword search. Never writes.
func relatedPages(ctx context.Context, deps ToolsDeps, agentID, draft, pageID string) []storePage {
	seen := map[string]bool{}
	var out []storePage
	if pageID != "" {
		if p, err := deps.Store.GetPage(ctx, agentID, pageID); err == nil && p != nil {
			out = append(out, storePage{ID: p.ID, Title: p.Title, BodyMD: p.BodyMD, Status: p.Status})
			seen[p.ID] = true
		}
	}
	hits, err := semanticSearch(ctx, deps, agentID, draft, 5)
	if err != nil || len(hits) == 0 {
		kw, kerr := keywordSearch(ctx, deps, agentID, draft, 5)
		if kerr == nil && len(kw) > 0 {
			hits = kw
		}
	}
	for _, h := range hits {
		if seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		p, err := deps.Store.GetPage(ctx, agentID, h.ID)
		if err != nil || p == nil {
			continue
		}
		out = append(out, storePage{ID: p.ID, Title: p.Title, BodyMD: p.BodyMD, Status: p.Status})
	}
	return out
}

// storePage is a lightweight page projection for review prompts.
type storePage struct {
	ID     string
	Title  string
	BodyMD string
	Status string
}
