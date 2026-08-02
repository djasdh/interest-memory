package wiki

import (
	"context"
	"encoding/json"
	"testing"

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
	resp  any
	err   error
	calls int
}

func (f *fakeLLM) ChatJSON(_ context.Context, _ []llm.Message, out any) error {
	f.calls++
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

func TestVerifyClaimsToolDegradesWithoutLLM(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	tool := NewVerifyClaimsTool(deps)
	if _, err := tool.Execute(types.Context{}, types.ArgsMap{"text": "x"}, nil); err != nil {
		t.Fatalf("expected degraded no-error, got %v", err)
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

	w := NewWriter(deps, nil)
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
	pts := []store.InterestPoint{{Name: "Go concurrency", Summary: "use goroutines", Keywords: []string{"go"}}}
	prompt := buildCompilePrompt(pts, nil)
	if prompt == "" || prompt == "(wiki: no relevant articles found)" {
		t.Fatal("empty prompt")
	}
	if !contains(prompt, "Go concurrency") {
		t.Errorf("prompt missing topic: %s", prompt)
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
