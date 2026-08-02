package wiki

import (
	"context"
	"strings"
	"testing"

	"interest-memory/internal/store"

	"my-agent-core/provider"
	"my-agent-core/types"
)

func seedPages(t *testing.T, deps ToolsDeps, agentID string, pages map[string]string, edges [][3]string) {
	t.Helper()
	ctx := context.Background()
	for id, body := range pages {
		if err := deps.Store.UpsertPage(ctx, store.Page{ID: id, AgentID: agentID, Title: id, BodyMD: body, Status: "active"}); err != nil {
			t.Fatalf("seed page %s: %v", id, err)
		}
	}
	for _, e := range edges {
		if err := deps.Store.AddEdgePair(ctx, agentID, store.Edge{SourceID: e[0], TargetID: e[1], Kind: store.EdgeType(e[2]), Weight: 1}); err != nil {
			t.Fatalf("seed edge %s: %v", e, err)
		}
	}
}

func TestCollectRelatedWalksHops(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	seedPages(t, deps, "a", map[string]string{
		"page-a": "A content", "page-b": "B content", "page-c": "C content",
	}, [][3]string{
		{"page-a", "page-b", "related"},
		{"page-b", "page-c", "related"},
	})
	w := &Writer{deps: deps}

	got, err := w.collectRelated(context.Background(), "a", []string{"page-a"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, p := range got {
		ids[p.ID] = true
	}
	for _, want := range []string{"page-a", "page-b", "page-c"} {
		if !ids[want] {
			t.Errorf("related pages missing %q: %v", want, ids)
		}
	}
}

func TestCollectRelatedHopsLimit(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	seedPages(t, deps, "a", map[string]string{
		"page-a": "A", "page-b": "B", "page-c": "C",
	}, [][3]string{
		{"page-a", "page-b", "related"},
		{"page-b", "page-c", "related"},
	})
	w := &Writer{deps: deps}
	got, _ := w.collectRelated(context.Background(), "a", []string{"page-a"}, 1)
	ids := map[string]bool{}
	for _, p := range got {
		ids[p.ID] = true
	}
	if !ids["page-a"] || !ids["page-b"] || ids["page-c"] {
		t.Errorf("hops=1 related = %v, want a+b not c", ids)
	}
}

func TestCollectRelatedResolvesArchivedPointToConceptPage(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	seedPages(t, deps, "a", map[string]string{"concept-x": "X content"}, [][3]string{
		{"ip-1", "concept-x", "has_page"},
	})
	w := &Writer{deps: deps}
	got, err := w.collectRelated(context.Background(), "a", []string{"ip-1"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range got {
		if p.ID == "concept-x" {
			found = true
		}
	}
	if !found {
		t.Errorf("archived point should resolve to concept page, got %v", got)
	}
}

func TestReconcileBatchesOverTen(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	pages := map[string]string{}
	var edges [][3]string
	for i := 0; i < 15; i++ {
		id := "page-" + string(rune('a'+i))
		pages[id] = id
		edges = append(edges, [3]string{"page-a", id, "related"})
	}
	seedPages(t, deps, "a", pages, edges)
	runner := &fakeRunner{}
	w := &Writer{deps: deps, runLoop: runner.run}
	model := types.Model{ID: "m", BaseURL: "http://127.0.0.1:9/v1", API: provider.APIOpenAICompletions}
	w.prov = func(context.Context) (*provider.Provider, error) {
		return provider.NewConfiguredProvider(model, "k"), nil
	}

	err := w.ReconcileRelated(context.Background(), "a", ReconcileInput{TouchedPages: []string{"page-a"}}, 2, 10)
	if err != nil {
		t.Fatalf("ReconcileRelated: %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("loop batches = %d, want 2 (15 pages / 10)", runner.calls)
	}
}

func TestReconcilePromptMentionsChanges(t *testing.T) {
	in := ReconcileInput{TouchedPages: []string{"page-a"}, ArchivedPoints: []string{"ip-1"}}
	batch := []store.Page{{ID: "page-b", Title: "B", Status: "active", BodyMD: "old"}}
	prompt := buildReconcilePrompt(in, batch)
	for _, want := range []string{"page-a", "ip-1", "page-b", "superseded"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("reconcile prompt missing %q\n---\n%s", want, prompt)
		}
	}
}
