package wiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/websearch"

	"github.com/djasdh/my-agent-core/types"
)

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ClaimLLM is the chat surface verify_claims needs (implemented by
// *llm.Client). Kept narrow for test fakes.
type ClaimLLM interface {
	ChatJSON(ctx context.Context, messages []llm.Message, out any) error
}

// ToolsDeps is everything the wiki tools need beyond the agent loop.
// Search/LLM are optional: when absent, verify_claims degrades gracefully.
type ToolsDeps struct {
	Store    store.Store
	Vec      vec.VectorIndex
	Embedder Embedder
	Search   websearch.Searcher
	LLM      ClaimLLM
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
	// Byte-level cuts can split a multi-byte UTF-8 rune; back off to the
	// previous rune boundary so wiki tool output stays valid UTF-8.
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		_, size := utf8.DecodeLastRuneInString(cut)
		cut = cut[:len(cut)-size]
	}
	return cut + "..."
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
				"title":      map[string]any{"type": "string"},
				"content":    map[string]any{"type": "string", "description": "Page content in markdown, may include [[wikilinks]] to other pages"},
				"page_type":  map[string]any{"type": "string", "description": "concept | source | synthesis | entity"},
				"status":     map[string]any{"type": "string", "description": "active | superseded | archived (default active)"},
				"event_time": map[string]any{"type": "string", "description": "Event time (RFC3339, from the interest point's session start time)"},
				"interest_point_ids": map[string]any{
					"type":        "array",
					"description": "The interest point id(s) that drove this page — links the page to them via has_page edges (multi-to-one allowed)",
					"items":       map[string]any{"type": "string"},
				},
				"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Taxonomy tags (look up existing tags with wiki_tags and reuse them)"},
				"sources":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Sources: key web page URLs or existing page ids (may be omitted for subjective interest points)"},
				"session_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "source session ids (for source pages)"},
				"edges": map[string]any{
					"type":        "array",
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

	status := strings.ToLower(asString(args["status"]))
	switch status {
	case "active", "superseded", "archived":
	default:
		status = "active"
	}
	ipIDs := stringList(args["interest_point_ids"])
	for i := range ipIDs {
		ipIDs[i] = normalizeID(ipIDs[i])
	}
	var eventTime time.Time
	if s := asString(args["event_time"]); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			eventTime = t.UTC()
		}
	}

	tags := stringList(args["tags"])
	if len(tags) > 10 {
		tags = tags[:10]
	}
	sources := stringList(args["sources"])
	if len(sources) > 10 {
		sources = sources[:10]
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
		Status:    status,
		Tags:      tags,
		Sources:   sources,
		EventTime: eventTime,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing != nil {
		page.CreatedAt = existing.CreatedAt
	}
	if err := deps.Store.UpsertPage(ctx, page); err != nil {
		return "", fmt.Errorf("wiki_write: upsert page: %w", err)
	}

	// Link the interest points that drove this page (has_page edges).
	var logEdges []store.LogEdge
	for _, ipID := range ipIDs {
		if err := deps.Store.AddEdgePair(ctx, agentID, store.Edge{
			SourceID: ipID, TargetID: id,
			Kind: store.EdgeHasPage, Weight: 1, CreatedAt: now,
		}); err != nil {
			return "", fmt.Errorf("wiki_write: add has_page edge %s→%s: %w", ipID, id, err)
		}
		logEdges = append(logEdges, store.LogEdge{Action: "add", SourceID: ipID, TargetID: id, Kind: store.EdgeHasPage, Weight: 1})
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

	// Change log: page action + structural edges (has_page/related/
	// contradicts/sequel; references excluded).
	logAction := "create"
	if existing != nil {
		logAction = "update"
	}
	if status != "active" {
		logAction = status // superseded | archived
	}
	for _, e := range edges {
		kind := store.EdgeType(e.Type)
		if kind != store.EdgeHasPage && kind != store.EdgeRelated && kind != store.EdgeContradict && kind != store.EdgeSequel {
			continue
		}
		logEdges = append(logEdges, store.LogEdge{Action: "add", SourceID: id, TargetID: e.TargetID, Kind: kind, Weight: 1})
	}
	_ = deps.Store.AppendLog(ctx, store.ChangeLog{
		AgentID: agentID, EntityKind: "wiki_page", EntityID: id, Title: title,
		Action: logAction, Edges: logEdges,
	})

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

// verifyResult is the LLM's JSON verdict for one claim audit.
type verifyResult struct {
	Status     string   `json:"status"` // supported | contested | unknown
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence"`
	Reason     string   `json:"reason"`
}

// NewVerifyClaimsTool returns the verify_claims tool: the agent calls it
// during writing to fact-check a draft statement against the web. It gathers
// web evidence (when a Searcher is wired) then asks the LLM for a verdict.
// Degrades to a JSON unknown verdict rather than erroring. lang is the output
// language for the audit prompt.
func NewVerifyClaimsTool(deps ToolsDeps, lang string) types.Tool {
	return types.Tool{
		Name:        "verify_claims",
		Description: "Fact-check a statement against the web before writing it to the wiki. Use this for objective factual claims you are about to persist; returns a JSON verdict (supported/contested/unknown) with evidence. Subjective preferences do not need this.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{
					"type":        "string",
					"description": "The factual statement/claim to verify",
				},
			},
			"required": []string{"text"},
		},
		Execute: func(_ types.Context, args types.ArgsMap, _ <-chan struct{}) (string, error) {
			text, _ := args["text"].(string)
			if strings.TrimSpace(text) == "" {
				return "", fmt.Errorf("verify_claims: missing 'text'")
			}
			return auditClaims(context.Background(), deps, text, lang)
		},
	}
}

func auditClaims(ctx context.Context, deps ToolsDeps, text, lang string) (string, error) {
	var evidence []websearch.SearchItem
	if deps.Search != nil {
		items, err := deps.Search.Search(ctx, text, 5)
		if err == nil {
			evidence = items
		}
	}
	if deps.LLM == nil {
		// No verdict capability: return a degraded JSON so the agent knows
		// the statement was not web-verified.
		return `{"status":"unknown","confidence":0,"evidence":[],"reason":"verify_claims: no LLM configured"}` + "\n", nil
	}

	if lang == "" {
		lang = "English"
	}
	var b strings.Builder
	b.WriteString("Judge whether the following statement is reliable based on web evidence (or general knowledge if none).\n\n")
	b.WriteString("Statement: " + text + "\n")
	if len(evidence) > 0 {
		b.WriteString("\nWeb evidence:\n")
		for i, e := range evidence {
			b.WriteString(fmt.Sprintf("%d. %s — %s (%s)\n", i+1, e.Title, truncate(e.Snippet, 200), e.URL))
		}
	} else {
		b.WriteString("\n(No web evidence — rely on the statement wording and general knowledge.)\n")
	}
	b.WriteString(`
Return ONLY valid JSON:
{
  "status": "supported" | "contested" | "unknown",
  "confidence": 0.0-1.0,
  "evidence": ["short reason 1", "short reason 2"],
  "reason": "one sentence verdict"
}`)
	b.WriteString(fmt.Sprintf("\n\nWrite evidence and reason in '%s'.\n", lang))

	var vr verifyResult
	if err := deps.LLM.ChatJSON(ctx, []llm.Message{{Role: "user", Content: b.String()}}, &vr); err != nil {
		return `{"status":"unknown","confidence":0,"evidence":[],"reason":"verify_claims: verdict failed"}` + "\n", nil
	}
	switch vr.Status {
	case "supported", "contested", "unknown":
	default:
		vr.Status = "unknown"
	}
	if vr.Confidence <= 0 || vr.Confidence > 1 {
		vr.Confidence = 0
	}
	out, _ := json.Marshal(vr)
	return string(out) + "\n", nil
}

// NewTagsTool returns the wiki_tags tool: a read-only listing of all tags
// already used by this agent (with usage counts). The agent consults it
// before writing so it reuses the existing tag taxonomy instead of inventing
// near-duplicate tags.
func NewTagsTool(deps ToolsDeps, agentID string) types.Tool {
	return types.Tool{
		Name:        "wiki_tags",
		Description: "List all tags already used in this agent's wiki (with usage counts). Call this before writing to reuse existing tags from the taxonomy instead of creating duplicates. Read-only.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(_ types.Context, _ types.ArgsMap, _ <-chan struct{}) (string, error) {
			tags, err := deps.Store.ListTags(context.Background(), agentID)
			if err != nil {
				return "", fmt.Errorf("wiki_tags: %w", err)
			}
			if len(tags) == 0 {
				return "(wiki: no tags used yet — you may introduce new tags)\n", nil
			}
			var b strings.Builder
			b.WriteString("Existing tags (count):\n")
			for _, tc := range tags {
				b.WriteString(fmt.Sprintf("- %s (%d)\n", tc.Tag, tc.Count))
			}
			return b.String(), nil
		},
	}
}

// claimID derives a stable id for a claim from its page + text.
func claimID(agentID, pageID, text string) string {
	h := sha256.Sum256([]byte(agentID + "|" + pageID + "|" + text))
	return hex.EncodeToString(h[:])[:16]
}

// NewIPQueryTool returns the ip_query tool: semantic search over interest
// points only (wiki pages excluded), with has_page relationships resolved for
// each hit. Scope includes the historical library (vec.Search is the whole
// agent namespace). Keyword fallback when vectors are unavailable.
func NewIPQueryTool(deps ToolsDeps, agentID string) types.Tool {
	return types.Tool{
		Name:        "ip_query",
		Description: "Search interest points (not wiki pages) related to the given query. Returns each point's name/summary/reliability and the wiki page(s) it already has (has_page). Use to check which interest points already exist and whether they already have a page before writing.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "The search query (natural language)"},
				"top_k": map[string]any{"type": "number", "description": "Number of results to return (default 3, max 10)"},
			},
			"required": []string{"query"},
		},
		Execute: func(_ types.Context, args types.ArgsMap, _ <-chan struct{}) (string, error) {
			query, _ := args["query"].(string)
			if strings.TrimSpace(query) == "" {
				return "", fmt.Errorf("ip_query: missing 'query'")
			}
			topK := 3
			if v, ok := args["top_k"].(float64); ok && v > 0 {
				topK = int(v)
			}
			if topK > 10 {
				topK = 10
			}
			return queryInterestPoints(context.Background(), deps, agentID, query, topK)
		},
	}
}

func queryInterestPoints(ctx context.Context, deps ToolsDeps, agentID, query string, topK int) (string, error) {
	var ipIDs []string
	if deps.Embedder != nil && deps.Vec != nil {
		if q, err := deps.Embedder.Embed(ctx, query); err == nil {
			if hits, err := deps.Vec.Search(ctx, agentID, q, topK); err == nil {
				for _, h := range hits {
					if h.Kind == "interest_point" {
						ipIDs = append(ipIDs, h.ID)
					}
				}
			}
		}
	}
	if len(ipIDs) == 0 {
		ips, err := deps.Store.SearchInterestPointsByKeywords(ctx, agentID, query, topK)
		if err != nil || len(ips) == 0 {
			return "(ip_query: no matching interest points)", nil
		}
		for _, p := range ips {
			ipIDs = append(ipIDs, p.ID)
		}
	}
	if len(ipIDs) == 0 {
		return "(ip_query: no matching interest points)", nil
	}

	pts, err := deps.Store.GetInterestPointsByIDs(ctx, agentID, ipIDs)
	if err != nil {
		return "", fmt.Errorf("ip_query: get interest points: %w", err)
	}
	byID := make(map[string]store.InterestPoint, len(pts))
	for _, p := range pts {
		byID[p.ID] = p
	}
	pages, _ := deps.Store.InterestPointPages(ctx, agentID, ipIDs)
	pagesOf := make(map[string][]string)
	for _, r := range pages {
		pagesOf[r.InterestPointID] = append(pagesOf[r.InterestPointID], r.PageID+" ("+r.PageTitle+")")
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Found %d interest point(s) for %q:\n\n", len(ipIDs), query))
	n := 0
	for _, id := range ipIDs {
		p, ok := byID[id]
		if !ok {
			continue
		}
		n++
		b.WriteString(fmt.Sprintf("%d. %s\n", n, p.Name))
		if p.Summary != "" {
			b.WriteString(fmt.Sprintf("   Summary: %s\n", truncate(p.Summary, 300)))
		}
		if p.Reliability.Status != "" {
			b.WriteString(fmt.Sprintf("   Reliability: %s (%.2f)\n", p.Reliability.Status, p.Reliability.Confidence))
		}
		if ps := pagesOf[id]; len(ps) > 0 {
			b.WriteString("   Existing page(s): " + strings.Join(ps, "; ") + "\n")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}
