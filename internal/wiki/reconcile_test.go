package wiki

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/djasdh/interest-memory/internal/store"

	"github.com/djasdh/my-agent-core/provider"
	"github.com/djasdh/my-agent-core/types"
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
	prompt := buildReconcilePrompt(in, []ArchivedInfo{{ID: "ip-1", Title: "旧点", Superseded: true, ReplacementID: "ip-2", ReplacementTitle: "新点"}}, batch, "English")
	for _, want := range []string{"page-a", "ip-1", "ip-2", "page-b", "Replacement"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("reconcile prompt missing %q\n---\n%s", want, prompt)
		}
	}
}

func TestReconcilePromptDeleteShowsOutlinks(t *testing.T) {
	in := ReconcileInput{ArchivedPoints: []string{"ip-1"}}
	batch := []store.Page{{ID: "page-b", Title: "B", Status: "active"}}
	archived := []ArchivedInfo{{
		ID:    "ip-1",
		Title: "旧点",
		Outlinks: []store.Edge{
			{SourceID: "ip-1", TargetID: "page-x", Kind: store.EdgeHasPage},
			{SourceID: "ip-1", TargetID: "ip-2", Kind: store.EdgeRelated},
		},
	}}
	prompt := buildReconcilePrompt(in, archived, batch, "English")
	for _, want := range []string{"Deletion", "ip-1", "has_page→page-x", "related→ip-2"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("delete reconcile prompt missing %q\n---\n%s", want, prompt)
		}
	}
}

func TestAllArchived(t *testing.T) {
	if allArchived([]store.Page{{Status: "superseded"}, {Status: "archived"}}) != true {
		t.Error("all archived pages should be allArchived")
	}
	if allArchived([]store.Page{{Status: "active"}}) != false {
		t.Error("active page should not be allArchived")
	}
	if allArchived(nil) != true {
		t.Error("empty set should be allArchived (no content)")
	}
}

func TestDescribeArchived(t *testing.T) {
	s := describeArchived(ArchivedInfo{ID: "ip-1", Title: "旧", Superseded: true, ReplacementID: "ip-2", ReplacementTitle: "新"})
	if !strings.Contains(s, "replacement chain") || !strings.Contains(s, "ip-1") || !strings.Contains(s, "ip-2") {
		t.Errorf("superseded describe missing replacement chain: %q", s)
	}
	d := describeArchived(ArchivedInfo{ID: "ip-1", Outlinks: []store.Edge{{TargetID: "p1", Kind: store.EdgeHasPage}}})
	if !strings.Contains(d, "original outlinks") || !strings.Contains(d, "p1") {
		t.Errorf("deleted describe missing outlinks: %q", d)
	}
}

func TestApplyCodeFallbackMarksSupersededAndRewritesLinks(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	ctx := context.Background()
	now := time.Now()

	// Old chain: ip-old --has_page--> pg-old, ip-old --sequel--> ip-new,
	// ip-new --has_page--> pg-new. A backlink page references [[pg-old]].
	deps.Store.UpsertInterestPoint(ctx, store.InterestPoint{ID: "ip-old", AgentID: "a", Name: "旧", Status: "archived"})
	deps.Store.UpsertInterestPoint(ctx, store.InterestPoint{ID: "ip-new", AgentID: "a", Name: "新", Status: "active"})
	deps.Store.UpsertPage(ctx, store.Page{ID: "pg-old", AgentID: "a", Title: "旧页", Status: "active", CreatedAt: now, UpdatedAt: now})
	deps.Store.UpsertPage(ctx, store.Page{ID: "pg-new", AgentID: "a", Title: "新页", Status: "active", CreatedAt: now, UpdatedAt: now})
	deps.Store.UpsertPage(ctx, store.Page{ID: "pg-back", AgentID: "a", Title: "引用页", Status: "active",
		BodyMD: "见 [[pg-old]] 说明", CreatedAt: now, UpdatedAt: now})
	deps.Store.AddEdgePair(ctx, "a", store.Edge{SourceID: "ip-old", TargetID: "pg-old", Kind: store.EdgeHasPage, Weight: 1})
	deps.Store.AddEdgePair(ctx, "a", store.Edge{SourceID: "ip-old", TargetID: "ip-new", Kind: store.EdgeSequel, Weight: 1})
	deps.Store.AddEdgePair(ctx, "a", store.Edge{SourceID: "ip-new", TargetID: "pg-new", Kind: store.EdgeHasPage, Weight: 1})
	deps.Store.AddEdgePair(ctx, "a", store.Edge{SourceID: "pg-back", TargetID: "pg-old", Kind: store.EdgeReference, Weight: 1})

	w := &Writer{deps: deps}
	archived := []ArchivedInfo{{ID: "ip-old", Superseded: true, ReplacementID: "ip-new"}}
	if err := w.applyCodeFallback(ctx, "a", archived); err != nil {
		t.Fatal(err)
	}

	// Old page marked superseded.
	oldPg, _ := deps.Store.GetPage(ctx, "a", "pg-old")
	if oldPg == nil || oldPg.Status != "superseded" {
		t.Errorf("old page status = %+v, want superseded", oldPg)
	}
	// Backlink wikilink rewritten to the new page.
	back, _ := deps.Store.GetPage(ctx, "a", "pg-back")
	if back == nil || !strings.Contains(back.BodyMD, "[[pg-new]]") || strings.Contains(back.BodyMD, "[[pg-old]]") {
		t.Errorf("backlink body = %q, want rewritten [[pg-new]]", back.BodyMD)
	}
}

func TestReconcileSilentWhenAllRelatedArchived(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	ctx := context.Background()
	now := time.Now()

	deps.Store.UpsertPage(ctx, store.Page{ID: "pg-a", AgentID: "a", Title: "A", Status: "superseded", CreatedAt: now, UpdatedAt: now})
	deps.Store.AddEdgePair(ctx, "a", store.Edge{SourceID: "pg-a", TargetID: "pg-b", Kind: store.EdgeRelated, Weight: 1})
	deps.Store.UpsertPage(ctx, store.Page{ID: "pg-b", AgentID: "a", Title: "B", Status: "archived", CreatedAt: now, UpdatedAt: now})

	runner := &fakeRunner{}
	w := &Writer{deps: deps, runLoop: runner.run}
	model := types.Model{ID: "m", BaseURL: "http://127.0.0.1:9/v1", API: provider.APIOpenAICompletions}
	w.prov = func(context.Context) (*provider.Provider, error) {
		return provider.NewConfiguredProvider(model, "k"), nil
	}

	err := w.ReconcileRelated(ctx, "a", ReconcileInput{TouchedPages: []string{"pg-a"}}, 3, 10)
	if err != nil {
		t.Fatalf("ReconcileRelated: %v", err)
	}
	if runner.calls != 0 {
		t.Errorf("loop calls = %d, want 0 (all related archived → silent)", runner.calls)
	}
}

func TestCollectRelatedDedupesMultiHasPage(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	ctx := context.Background()
	if err := deps.Store.UpsertInterestPoint(ctx, store.InterestPoint{ID: "ip-1", AgentID: "agent-a", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.UpsertInterestPoint(ctx, store.InterestPoint{ID: "ip-2", AgentID: "agent-a", Name: "P2"}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.UpsertPage(ctx, store.Page{ID: "shared-page", AgentID: "agent-a", Title: "Shared", BodyMD: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.AddEdgePair(ctx, "agent-a", store.Edge{SourceID: "ip-1", TargetID: "shared-page", Kind: store.EdgeHasPage}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.AddEdgePair(ctx, "agent-a", store.Edge{SourceID: "ip-2", TargetID: "shared-page", Kind: store.EdgeHasPage}); err != nil {
		t.Fatal(err)
	}
	w := &Writer{deps: deps}
	pages, err := w.collectRelated(ctx, "agent-a", []string{"ip-1", "ip-2"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range pages {
		if p.ID == "shared-page" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shared-page appears %d times in related, want 1 (deduped multi-to-one)", count)
	}
}
