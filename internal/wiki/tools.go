package wiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"interest-memory/internal/store"
	"interest-memory/internal/vec"

	"my-agent-core/types"
)

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ToolsDeps is everything the wiki tools need beyond the agent loop.
type ToolsDeps struct {
	Store    store.Store
	Vec      vec.VectorIndex
	Embedder Embedder
}

// validEdgeKinds maps the store's five edge kinds.
var validEdgeKinds = map[store.EdgeType]bool{
	store.EdgeRelated:    true,
	store.EdgeContradict: true,
	store.EdgeSequel:     true,
	store.EdgeReference:  true,
	store.EdgeHasPage:    true,
}

// NewQueryTool returns the wiki_query tool: semantic search over the agent's
// wiki pages + interest points, with keyword fallback.
func NewQueryTool(deps ToolsDeps, agentID string) types.Tool {
	return types.Tool{
		Name:        "wiki_query",
		Description: "Search the wiki for articles and interest points related to the given query. Use this to check what knowledge already exists before writing.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query (natural language)",
				},
				"top_k": map[string]any{
					"type":        "number",
					"description": "Number of results to return (default 3, max 10)",
				},
			},
			"required": []string{"query"},
		},
		Execute: func(_ types.Context, args types.ArgsMap, _ <-chan struct{}) (string, error) {
			query, _ := args["query"].(string)
			if strings.TrimSpace(query) == "" {
				return "", fmt.Errorf("wiki_query: missing 'query'")
			}
			topK := 3
			if v, ok := args["top_k"].(float64); ok && v > 0 {
				topK = int(v)
			}
			if topK > 10 {
				topK = 10
			}
			return queryWiki(context.Background(), deps, agentID, query, topK)
		},
	}
}

func queryWiki(ctx context.Context, deps ToolsDeps, agentID, query string, topK int) (string, error) {
	hits, err := semanticSearch(ctx, deps, agentID, query, topK)
	if err != nil || len(hits) == 0 {
		kw, kerr := keywordSearch(ctx, deps, agentID, query, topK)
		if kerr == nil && len(kw) > 0 {
			hits = kw
		}
	}
	if len(hits) == 0 {
		return "(wiki: no relevant articles found)", nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Found %d relevant wiki record(s) for %q:\n\n", len(hits), query))
	for i, h := range hits {
		b.WriteString(fmt.Sprintf("=== %d. %s (score %.2f) [%s] ===\n", i+1, titleOf(h), h.Score, h.Kind))
		b.WriteString(fmt.Sprintf("ID: %s\n", h.ID))
		if body, ok := h.Meta["body"]; ok && body != "" {
			b.WriteString(fmt.Sprintf("Preview: %s\n", truncate(body, 400)))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func semanticSearch(ctx context.Context, deps ToolsDeps, agentID, query string, topK int) ([]vec.Hit, error) {
	if deps.Embedder == nil || deps.Vec == nil {
		return nil, nil
	}
	q, err := deps.Embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	return deps.Vec.Search(ctx, agentID, q, topK)
}

func keywordSearch(ctx context.Context, deps ToolsDeps, agentID, query string, topK int) ([]vec.Hit, error) {
	if deps.Vec == nil {
		return nil, nil
	}
	return deps.Vec.SearchByKeywords(ctx, agentID, query, topK)
}

func titleOf(h vec.Hit) string {
	if h.Meta != nil {
		if t, ok := h.Meta["title"]; ok && t != "" {
			return t
		}
	}
	return h.ID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// edgeArg is one edge supplied by the agent.
type edgeArg struct {
	TargetID string `json:"target_id"`
	Type     string `json:"type"`
}

type claimArg struct {
	Text       string   `json:"text"`
	Confidence float64  `json:"confidence"`
	Status     string   `json:"status"`
	Evidence   []string `json:"evidence"`
}

// NewWriteTool returns the wiki_write tool: creates or updates a wiki page
// (with claims, edges, and embedding) in the SQLite store. Enforces edge
// rules before writing (target exists / no self-ref / max 8 / dedup / kind).
func NewWriteTool(deps ToolsDeps, agentID string) types.Tool {
	return types.Tool{
		Name:        "wiki_write",
		Description: "Create or update a wiki page. Use this to persist knowledge from conversations. Provide page_type (concept/source/synthesis/entity), title, content in markdown, optional edges, and optional claims.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "Unique page identifier (lowercase, hyphen-separated)",
				},
				"title":       map[string]any{"type": "string"},
				"content":     map[string]any{"type": "string", "description": "Page content in markdown, may include [[wikilinks]] to other pages"},
				"page_type":   map[string]any{"type": "string", "description": "concept | source | synthesis | entity"},
				"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"session_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "source session ids (for source pages)"},
				"edges": map[string]any{
					"type":  "array",
					"description": "Edges to other pages. Each: {target_id, type} where type is one of: related, contradicts, sequel, references, has_page",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"target_id": map[string]any{"type": "string"},
							"type":      map[string]any{"type": "string"},
						},
					},
				},
				"claims": map[string]any{
					"type":        "array",
					"description": "Optional structured claims backing this page",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text":       map[string]any{"type": "string"},
							"confidence": map[string]any{"type": "number"},
							"status":     map[string]any{"type": "string"},
							"evidence":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					},
				},
			},
			"required": []string{"id", "title", "content"},
		},
		Execute: func(_ types.Context, args types.ArgsMap, _ <-chan struct{}) (string, error) {
			return writeWiki(context.Background(), deps, agentID, args)
		},
	}
}

func writeWiki(ctx context.Context, deps ToolsDeps, agentID string, args types.ArgsMap) (string, error) {
	id, _ := args["id"].(string)
	title, _ := args["title"].(string)
	content, _ := args["content"].(string)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(title) == "" {
		return "", fmt.Errorf("wiki_write: 'id' and 'title' are required")
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("wiki_write: 'content' is required")
	}

	id = normalizeID(id)
	pageType := store.PageType(strings.ToLower(asString(args["page_type"])))
	switch pageType {
	case store.PageEntity, store.PageConcept, store.PageSynthesis, store.PageSource:
	default:
		pageType = store.PageConcept
	}

	tags := stringList(args["tags"])
	if len(tags) > 10 {
		tags = tags[:10]
	}
	sessions := stringList(args["session_ids"])
	if pageType == store.PageSource && len(sessions) > 0 {
		content = "> Source sessions: " + strings.Join(sessions, ", ") + "\n\n" + content
	}

	edges, err := edgeList(args["edges"])
	if err != nil {
		return "", err
	}
	claims, err := claimList(args["claims"])
	if err != nil {
		return "", err
	}

	// Existence + edge rules.
	existing, err := deps.Store.GetPage(ctx, agentID, id)
	if err != nil {
		return "", fmt.Errorf("wiki_write: get: %w", err)
	}
	if existing != nil {
		for _, e := range edges {
			if e.TargetID == id {
				return "", fmt.Errorf("wiki_write: page %q cannot reference itself", id)
			}
		}
	}
	if err := validateEdges(ctx, deps, agentID, id, edges); err != nil {
		return "", err
	}

	now := timeNow()
	page := store.Page{
		ID:        id,
		AgentID:   agentID,
		PageType:  pageType,
		Title:     title,
		BodyMD:    content,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing != nil {
		page.CreatedAt = existing.CreatedAt
	}
	if err := deps.Store.UpsertPage(ctx, page); err != nil {
		return "", fmt.Errorf("wiki_write: upsert page: %w", err)
	}

	// Claims.
	for _, c := range claims {
		status := c.Status
		switch status {
		case "supported", "contested", "stale":
		default:
			status = "supported"
		}
		var ev []store.Evidence
		for _, e := range c.Evidence {
			ev = append(ev, store.Evidence{Kind: "session", SourceID: asSession(agentID), Excerpt: truncate(e, 300)})
		}
		if err := deps.Store.UpsertClaim(ctx, store.Claim{
			ID:         claimID(agentID, id, c.Text),
			AgentID:    agentID,
			PageID:     id,
			Text:       c.Text,
			Status:     status,
			Confidence: clampConfidence(c.Confidence),
			Evidence:   ev,
			Freshness:  store.Freshness{Level: "unknown", UpdatedAt: now, TTLDays: 0},
		}); err != nil {
			return "", fmt.Errorf("wiki_write: upsert claim: %w", err)
		}
	}

	// Edges (AddEdgePair enforces contradicts bidirectional).
	for _, e := range edges {
		kind := store.EdgeType(e.Type)
		if err := deps.Store.AddEdgePair(ctx, agentID, store.Edge{
			SourceID:  id,
			TargetID:  e.TargetID,
			Kind:      kind,
			Weight:    1,
			CreatedAt: now,
		}); err != nil {
			return "", fmt.Errorf("wiki_write: add edge %s→%s: %w", id, e.TargetID, err)
		}
	}

	// Embedding (best-effort: failure does not abort the write).
	if deps.Embedder != nil && deps.Vec != nil {
		embedText := title + "\n" + content
		if len(embedText) > 8000 {
			embedText = embedText[:8000]
		}
		if v, err := deps.Embedder.Embed(ctx, embedText); err == nil {
			_ = deps.Vec.Upsert(ctx, vec.Entry{
				ID:      id,
				AgentID: agentID,
				Kind:    "wiki_page",
				Vector:  v,
				Metadata: map[string]string{
					"title": title,
					"body":  content,
				},
			})
		}
	}

	action := "Created"
	if existing != nil {
		action = "Updated"
	}
	return fmt.Sprintf("%s wiki page %q (%s, %s)", action, id, title, pageType), nil
}

// validateEdges enforces: target exists, kind valid, no duplicate, max 8.
func validateEdges(ctx context.Context, deps ToolsDeps, agentID, sourceID string, edges []edgeArg) error {
	if len(edges) > 8 {
		return fmt.Errorf("wiki_write: too many edges (%d), max 8", len(edges))
	}
	seen := map[string]bool{}
	for _, e := range edges {
		kind := store.EdgeType(e.Type)
		if !validEdgeKinds[kind] {
			return fmt.Errorf("wiki_write: invalid edge type %q", e.Type)
		}
		key := string(kind) + ":" + e.TargetID
		if seen[key] {
			return fmt.Errorf("wiki_write: duplicate edge %s→%s", sourceID, e.TargetID)
		}
		seen[key] = true
		if e.TargetID == sourceID {
			return fmt.Errorf("wiki_write: page %q cannot reference itself", sourceID)
		}
		target, err := deps.Store.GetPage(ctx, agentID, e.TargetID)
		if err != nil {
			return fmt.Errorf("wiki_write: lookup target: %w", err)
		}
		if target == nil {
			return fmt.Errorf("wiki_write: edge target %q does not exist", e.TargetID)
		}
	}
	return nil
}

func normalizeID(id string) string {
	id = strings.TrimSpace(strings.ToLower(id))
	id = strings.ReplaceAll(id, " ", "-")
	id = strings.ReplaceAll(id, "_", "-")
	return id
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func stringList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func edgeList(v any) ([]edgeArg, error) {
	raw, ok := v.([]any)
	if !ok {
		return nil, nil
	}
	var out []edgeArg
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		target, _ := m["target_id"].(string)
		typ, _ := m["type"].(string)
		if target == "" || typ == "" {
			continue
		}
		out = append(out, edgeArg{TargetID: normalizeID(target), Type: strings.ToLower(typ)})
	}
	return out, nil
}

func claimList(v any) ([]claimArg, error) {
	raw, ok := v.([]any)
	if !ok {
		return nil, nil
	}
	var out []claimArg
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text, _ := m["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, claimArg{
			Text:       text,
			Confidence: num(m["confidence"]),
			Status:     asString(m["status"]),
			Evidence:   stringList(m["evidence"]),
		})
	}
	return out, nil
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

func clampConfidence(c float64) float64 {
	if c <= 0 || c > 1 {
		return 0.5
	}
	return c
}

func asSession(agentID string) string { return agentID }

// claimID derives a stable id for a claim from its page + text.
func claimID(agentID, pageID, text string) string {
	h := sha256.Sum256([]byte(agentID + "|" + pageID + "|" + text))
	return hex.EncodeToString(h[:])[:16]
}
