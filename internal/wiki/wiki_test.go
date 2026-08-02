package wiki

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"interest-memory/internal/llm"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/websearch"

	"my-agent-core/types"
)

func newTestDeps(t *testing.T) (ToolsDeps, *store.SQLiteStore, vec.VectorIndex) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := vec.NewFallback(st.DB())
	if err != nil {
		t.Fatalf("vec: %v", err)
	}
	return ToolsDeps{Store: st, Vec: v, Embedder: fakeEmbedder{}}, st, v
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

type fakeSearcher struct {
	items []websearch.SearchItem
	err   error
	calls int
}

func (f *fakeSearcher) Search(_ context.Context, _ string, _ int) ([]websearch.SearchItem, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

type fakeLLM struct {
	resp       any
	err        error
	calls      int
	lastPrompt string
}

func (f *fakeLLM) ChatJSON(_ context.Context, msgs []llm.Message, out any) error {
	f.calls++
	if len(msgs) > 0 {
		f.lastPrompt = msgs[0].Content
	}
	if f.err != nil {
		return f.err
	}
	b, _ := json.Marshal(f.resp)
	return json.Unmarshal(b, out)
}

func TestVerifyClaimsToolAuditsText(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	se := &fakeSearcher{items: []websearch.SearchItem{{Title: "t", URL: "u", Snippet: "s"}}}
	l := &fakeLLM{resp: map[string]any{"status": "supported", "confidence": 0.9, "evidence": []string{"matches"}}}
	deps.Search = se
	deps.LLM = l

	tool := NewVerifyClaimsTool(deps)
	if tool.Name != "verify_claims" {
		t.Errorf("tool name = %q, want verify_claims", tool.Name)
	}
	out, err := tool.Execute(types.Context{}, types.ArgsMap{"text": "PostgreSQL 支持 JSONB"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if se.calls != 1 || l.calls != 1 {
		t.Errorf("searcher=%d llm=%d, want 1/1", se.calls, l.calls)
	}
	if out == "" {
		t.Fatal("empty output")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	if got["status"] != "supported" {
		t.Errorf("status = %v, want supported", got["status"])
	}
}

func TestVerifyClaimsToolDegradesWithoutSearch(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	l := &fakeLLM{resp: map[string]any{"status": "unknown"}}
	deps.LLM = l

	tool := NewVerifyClaimsTool(deps)
	out, err := tool.Execute(types.Context{}, types.ArgsMap{"text": "某个说法"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out == "" {
		t.Fatal("expected degraded (non-error) output")
	}
	if l.calls != 1 {
		t.Errorf("llm calls = %d, want 1 (verdict still asked)", l.calls)
	}
}

func TestReviewToolSuggestsAgainstExisting(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	// Seed an existing page about the same topic.
	if err := st.UpsertPage(context.Background(), store.Page{
		ID: "postgresql", AgentID: "a", Title: "PostgreSQL", BodyMD: "我们用 PostgreSQL 作为默认数据库",
		Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertEdge(context.Background(), "a", store.Edge{SourceID: "postgresql", TargetID: "x", Kind: store.EdgeRelated, Weight: 1})
	// Seed vec metadata so keyword search finds it.
	if err := deps.Vec.Upsert(context.Background(), vec.Entry{
		ID: "postgresql", AgentID: "a", Kind: "wiki_page",
		Metadata: map[string]string{"title": "PostgreSQL", "body": "我们用 PostgreSQL 作为默认数据库"},
	}); err != nil {
		t.Fatal(err)
	}
	l := &fakeLLM{resp: map[string]any{
		"summary": "建议核对",
		"suggestions": []map[string]any{
			{"type": "duplicate", "page_id": "postgresql", "message": "与现有页重复"},
		},
	}}
	deps.LLM = l

	tool := NewReviewTool(deps, "a")
	if tool.Name != "review" {
		t.Errorf("tool name = %q, want review", tool.Name)
	}
	out, err := tool.Execute(types.Context{}, types.ArgsMap{
		"draft": "PostgreSQL 作为默认数据库，JSONB 好用",
	}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if l.calls != 1 {
		t.Errorf("llm calls = %d, want 1", l.calls)
	}
	var got struct {
		Suggestion []struct {
			Type    string `json:"type"`
			PageID  string `json:"page_id"`
			Message string `json:"message"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	if len(got.Suggestion) != 1 || got.Suggestion[0].PageID != "postgresql" {
		t.Errorf("suggestions = %+v", got.Suggestion)
	}
	// Read-only: review must not write anything.
	p, err := st.GetPage(context.Background(), "a", "postgresql")
	if err != nil || p == nil || p.BodyMD != "我们用 PostgreSQL 作为默认数据库" {
		t.Errorf("review modified the store: page = %+v err=%v", p, err)
	}
}

func TestReviewToolDegradesWithoutLLM(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	tool := NewReviewTool(deps, "a")
	out, err := tool.Execute(types.Context{}, types.ArgsMap{"draft": "x"}, nil)
	if err != nil {
		t.Fatalf("expected degraded no-error, got %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty degraded output")
	}
}

func TestWriteToolCreatesHasPageEdge(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")

	_, err := tool.Execute(types.Context{}, map[string]any{
		"id":                "postgresql-page",
		"title":             "PostgreSQL",
		"content":           "默认数据库",
		"page_type":         "concept",
		"interest_point_id": "ip-123",
	}, nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	edges, err := st.Outlinks(ctx, "agent-a", "ip-123")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range edges {
		if e.TargetID == "postgresql-page" && e.Kind == store.EdgeHasPage {
			found = true
		}
	}
	if !found {
		t.Errorf("has_page edge not created; outlinks = %+v", edges)
	}
}

func TestWriteToolStatusParam(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")

	_, err := tool.Execute(types.Context{}, map[string]any{
		"id":      "old-page",
		"title":   "Old",
		"content": "旧内容",
		"status":  "superseded",
	}, nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	pg, err := st.GetPage(ctx, "agent-a", "old-page")
	if err != nil || pg == nil {
		t.Fatalf("page missing: %v", err)
	}
	if pg.Status != "superseded" {
		t.Errorf("page status = %q, want superseded", pg.Status)
	}
}

func TestWriteToolDefaultStatusActive(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")
	if _, err := tool.Execute(types.Context{}, map[string]any{
		"id": "fresh-page", "title": "Fresh", "content": "x",
	}, nil); err != nil {
		t.Fatal(err)
	}
	pg, _ := st.GetPage(ctx, "agent-a", "fresh-page")
	if pg == nil || pg.Status != "active" {
		t.Errorf("default status = %+v, want active", pg)
	}
}

func TestWriteToolLogsPageCreate(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	// Edge target must exist for validation.
	if err := st.UpsertPage(ctx, store.Page{ID: "pg2", AgentID: "agent-a", Title: "P2", BodyMD: "x"}); err != nil {
		t.Fatal(err)
	}
	tool := NewWriteTool(deps, "agent-a")
	if _, err := tool.Execute(types.Context{}, map[string]any{
		"id":                "pg1",
		"title":             "P1",
		"content":           "内容",
		"page_type":         "concept",
		"interest_point_id": "ip-1",
		"edges": []any{map[string]any{
			"target_id": "pg2",
			"type":      "related",
		}},
	}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	logs, err := st.ListLogs(ctx, "agent-a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	l := logs[0]
	if l.Action != "create" || l.EntityKind != "wiki_page" || l.EntityID != "pg1" || l.Title != "P1" {
		t.Errorf("log = %+v", l)
	}
	kinds := map[store.EdgeType]bool{}
	for _, e := range l.Edges {
		kinds[e.Kind] = true
	}
	if !kinds[store.EdgeHasPage] || !kinds[store.EdgeRelated] {
		t.Errorf("log edges = %+v, want has_page + related", l.Edges)
	}
}

func TestWriteToolLogsStatusChange(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")
	if _, err := tool.Execute(types.Context{}, map[string]any{
		"id": "old-page", "title": "Old", "content": "x", "status": "superseded",
	}, nil); err != nil {
		t.Fatal(err)
	}
	logs, _ := st.ListLogs(ctx, "agent-a", 0, 0)
	if len(logs) != 1 || logs[0].Action != "superseded" {
		t.Errorf("logs = %+v, want action=superseded", logs)
	}
}

func TestWriteToolPersistsTagsAndSources(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")
	if _, err := tool.Execute(types.Context{}, map[string]any{
		"id":      "pg-tag",
		"title":   "Tagged",
		"content": "内容",
		"tags":    []any{"go", "database"},
		"sources": []any{"https://x.example", "postgresql-page"},
	}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	pg, err := st.GetPage(ctx, "agent-a", "pg-tag")
	if err != nil || pg == nil {
		t.Fatalf("page missing: %v", err)
	}
	if len(pg.Tags) != 2 || pg.Tags[0] != "go" {
		t.Errorf("tags = %v, want [go database]", pg.Tags)
	}
	if len(pg.Sources) != 2 || pg.Sources[1] != "postgresql-page" {
		t.Errorf("sources = %v", pg.Sources)
	}
}

func TestWriteToolWithoutSourcesOK(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")
	if _, err := tool.Execute(types.Context{}, map[string]any{
		"id": "no-src", "title": "NoSrc", "content": "x",
	}, nil); err != nil {
		t.Fatalf("write without sources (subjective exemption): %v", err)
	}
	pg, _ := st.GetPage(ctx, "agent-a", "no-src")
	if pg == nil || len(pg.Sources) != 0 {
		t.Errorf("sources should be empty when not provided: %+v", pg)
	}
}

func TestTagsToolReturnsExistingTags(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	for i, tags := range [][]string{{"go", "database"}, {"go", "backend"}} {
		if err := st.UpsertPage(ctx, store.Page{ID: "pg" + string(rune('a'+i)), AgentID: "agent-a", Title: "T", BodyMD: "x", Tags: tags}); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewTagsTool(deps, "agent-a")
	if tool.Name != "wiki_tags" {
		t.Errorf("name = %q, want wiki_tags", tool.Name)
	}
	out, err := tool.Execute(types.Context{}, types.ArgsMap{}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"go", "database", "backend"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing tag %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2") {
		t.Errorf("output should show go count 2\n%s", out)
	}
}

func TestTagsToolEmptyOK(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	tool := NewTagsTool(deps, "agent-a")
	out, err := tool.Execute(types.Context{}, types.ArgsMap{}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out == "" {
		t.Fatal("empty output")
	}
}

func TestReviewToolInjectsTagsSourcesAndLinkCount(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	if err := st.UpsertPage(ctx, store.Page{ID: "postgresql-page", AgentID: "agent-a", Title: "PostgreSQL",
		BodyMD: "PostgreSQL 作为默认数据库", Status: "active", Tags: []string{"database"}, Sources: []string{"https://x.example"}}); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertEdge(ctx, "agent-a", store.Edge{SourceID: "postgresql-page", TargetID: "x", Kind: store.EdgeRelated, Weight: 1})
	_ = deps.Vec.Upsert(ctx, vec.Entry{ID: "postgresql-page", AgentID: "agent-a", Kind: "wiki_page",
		Metadata: map[string]string{"title": "PostgreSQL", "body": "PostgreSQL 作为默认数据库"}})
	l := &fakeLLM{resp: map[string]any{"summary": "ok", "suggestions": []any{}}}
	deps.LLM = l

	tool := NewReviewTool(deps, "agent-a")
	// Draft without any wikilink.
	if _, err := tool.Execute(types.Context{}, types.ArgsMap{"draft": "PostgreSQL 作为默认数据库"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"database", "https://x.example", "Draft contains 0 [[wikilink]]s"} {
		if !strings.Contains(l.lastPrompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, l.lastPrompt)
		}
	}
}

func TestWriteToolEventTimeParam(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")
	if _, err := tool.Execute(types.Context{}, map[string]any{
		"id": "pg-et", "title": "ET", "content": "x", "event_time": "2026-08-01T10:00:00Z",
	}, nil); err != nil {
		t.Fatal(err)
	}
	pg, _ := st.GetPage(ctx, "agent-a", "pg-et")
	if pg == nil {
		t.Fatal("page missing")
	}
	want := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	if !pg.EventTime.Equal(want) {
		t.Errorf("event_time = %v, want %v", pg.EventTime, want)
	}
}

func TestWriteToolCreateAndUpdate(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")

	res, err := tool.Execute(types.Context{}, map[string]any{
		"id":        "error-handling-patterns",
		"title":     "Error Handling Patterns",
		"content":   "Use structured errors.",
		"page_type": "concept",
		"tags":      []any{"go", "errors"},
	}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res == "" {
		t.Error("empty result message")
	}
	pg, err := st.GetPage(ctx, "agent-a", "error-handling-patterns")
	if err != nil || pg == nil {
		t.Fatalf("page not persisted: %v", err)
	}
	if pg.PageType != store.PageConcept || pg.Title != "Error Handling Patterns" {
		t.Errorf("page = %+v", pg)
	}

	// Update with edge target + claims.
	if err := st.UpsertPage(ctx, store.Page{ID: "related-page", AgentID: "agent-a",
		Title: "Related", PageType: store.PageConcept, BodyMD: "x"}); err != nil {
		t.Fatal(err)
	}
	res, err = tool.Execute(types.Context{}, map[string]any{
		"id":      "error-handling-patterns",
		"title":   "Error Handling Patterns v2",
		"content": "Use structured errors and wrap at boundaries.",
		"edges": []any{map[string]any{
			"target_id": "related-page",
			"type":      "related",
		}},
		"claims": []any{map[string]any{
			"text":       "Always wrap errors",
			"confidence": 0.9,
			"status":     "supported",
		}},
	}, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	pg, err = st.GetPage(ctx, "agent-a", "error-handling-patterns")
	if err != nil || pg == nil {
		t.Fatal("updated page missing")
	}
	if pg.Title != "Error Handling Patterns v2" {
		t.Errorf("title = %s", pg.Title)
	}
	edges, err := st.Outlinks(ctx, "agent-a", "error-handling-patterns")
	if err != nil || len(edges) != 1 {
		t.Fatalf("outlinks = %+v err=%v", edges, err)
	}
	if edges[0].TargetID != "related-page" || edges[0].Kind != store.EdgeRelated {
		t.Errorf("edge = %+v", edges[0])
	}
	claims, _ := st.ListClaims(ctx, "agent-a", "error-handling-patterns")
	if len(claims) != 1 || claims[0].Text != "Always wrap errors" {
		t.Errorf("claims = %+v", claims)
	}
}

func TestWriteToolEdgeRules(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	tool := NewWriteTool(deps, "agent-a")

	// Self-reference rejected.
	_, err := tool.Execute(types.Context{}, map[string]any{
		"id":      "page-a",
		"title":   "A",
		"content": "a",
		"edges":   []any{map[string]any{"target_id": "page-a", "type": "related"}},
	}, nil)
	if err == nil {
		t.Fatal("self-reference should be rejected")
	}

	// Nonexistent target rejected.
	if err := st.UpsertPage(ctx, store.Page{ID: "page-a", AgentID: "agent-a", Title: "A", BodyMD: "a"}); err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(types.Context{}, map[string]any{
		"id":      "page-b",
		"title":   "B",
		"content": "b",
		"edges":   []any{map[string]any{"target_id": "no-such-page", "type": "related"}},
	}, nil)
	if err == nil {
		t.Fatal("nonexistent target should be rejected")
	}

	// Invalid kind rejected.
	_, err = tool.Execute(types.Context{}, map[string]any{
		"id":      "page-c",
		"title":   "C",
		"content": "c",
		"edges":   []any{map[string]any{"target_id": "page-a", "type": "bogus"}},
	}, nil)
	if err == nil {
		t.Fatal("invalid edge kind should be rejected")
	}
}

func TestQueryToolReturnsResults(t *testing.T) {
	deps, st, v := newTestDeps(t)
	ctx := context.Background()
	// Seed a page and an index entry via fallback vec_meta.
	if err := st.UpsertPage(ctx, store.Page{ID: "page-a", AgentID: "agent-a",
		Title: "Go Concurrency", PageType: store.PageConcept, BodyMD: "channels and goroutines"}); err != nil {
		t.Fatal(err)
	}
	if err := v.Upsert(ctx, vec.Entry{ID: "page-a", AgentID: "agent-a", Kind: "wiki_page",
		Metadata: map[string]string{"title": "Go Concurrency", "body": "channels and goroutines"}}); err != nil {
		t.Fatal(err)
	}

	tool := NewQueryTool(deps, "agent-a")
	res, err := tool.Execute(types.Context{}, map[string]any{"query": "goroutines"}, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res == "" || res == "(wiki: no relevant articles found)" {
		t.Errorf("expected results, got %q", res)
	}
}

func TestRebuildEdgesFromWikilinks(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	now := timeNow()
	if err := st.UpsertPage(ctx, store.Page{ID: "page-a", AgentID: "agent-a",
		Title: "A", PageType: store.PageConcept, BodyMD: "links to [[page-b]] and [[page-c]]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPage(ctx, store.Page{ID: "page-b", AgentID: "agent-a",
		Title: "B", PageType: store.PageConcept, BodyMD: "b", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	w := NewWriter(deps, nil, "中文")
	if err := w.RebuildEdges(ctx, "agent-a"); err != nil {
		t.Fatalf("RebuildEdges: %v", err)
	}
	edges, err := st.Outlinks(ctx, "agent-a", "page-a")
	if err != nil {
		t.Fatal(err)
	}
	// page-c doesn't exist → only page-b edge survives.
	if len(edges) != 1 || edges[0].TargetID != "page-b" || edges[0].Kind != store.EdgeReference {
		t.Errorf("outlinks = %+v", edges)
	}
	// Dead link feedback: page-c is recorded as pending.
	pending, err := st.ListPendingLinks(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SourceID != "page-a" || pending[0].Target != "page-c" {
		t.Errorf("pending links = %+v, want page-a→page-c", pending)
	}

	// Resolving the target clears the pending record and adds the edge.
	if err := st.UpsertPage(ctx, store.Page{ID: "page-c", AgentID: "agent-a",
		Title: "C", PageType: store.PageConcept, BodyMD: "c", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := w.RebuildEdges(ctx, "agent-a"); err != nil {
		t.Fatalf("RebuildEdges (2nd): %v", err)
	}
	pending2, _ := st.ListPendingLinks(ctx, "agent-a")
	if len(pending2) != 0 {
		t.Errorf("pending after resolve = %+v, want empty", pending2)
	}
	edges2, _ := st.Outlinks(ctx, "agent-a", "page-a")
	if len(edges2) != 2 {
		t.Errorf("outlinks after resolve = %+v, want 2 (page-b + page-c)", edges2)
	}
}

func TestExtractWikilinks(t *testing.T) {
	body := `# Doc
See [[page-b]] and [[Page C|label]] and ![[img.png]] and [[#header]].
Also [[page-b]] again.
`
	links := ExtractWikilinks(body)
	if len(links) != 2 {
		t.Fatalf("links = %v, want 2 (page-b, Page C)", links)
	}
	if links[0] != "page-b" || links[1] != "Page C" {
		t.Errorf("links = %v", links)
	}
}

func TestBuildCompilePrompt(t *testing.T) {
	prompt := buildPointPrompt(store.InterestPoint{
		Name: "Go concurrency", Summary: "use goroutines", Keywords: []string{"go"},
		Subjective:  false,
		TurnRange:   [2]int{0, 0},
		Reliability: store.Reliability{Confidence: 0.9, Status: "supported"},
	}, "", "")
	if prompt == "" {
		t.Fatal("empty prompt")
	}
	if !contains(prompt, "Go concurrency") {
		t.Errorf("prompt missing topic: %s", prompt)
	}
	if !contains(prompt, "verify_claims") {
		t.Errorf("prompt should instruct web audit for objective claims: %s", prompt)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
