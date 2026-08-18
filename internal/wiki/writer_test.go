package wiki

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djasdh/interest-memory/internal/store"

	"github.com/djasdh/my-agent-core/provider"
	"github.com/djasdh/my-agent-core/types"
)

type fakeProviderFactory struct {
	calls int
}

func (f *fakeProviderFactory) get(context.Context) (*provider.Provider, error) {
	f.calls++
	// Real provider is only needed when a loop actually runs; tests inject a
	// fake runner so this should never be exercised.
	return nil, nil
}

// fakeRunner records loop invocations and can emit a wiki_write event.
// Compile runs loops concurrently, so counters are mutex-guarded.
type fakeRunner struct {
	mu    sync.Mutex
	calls int
	// prompts captures the prompt text of each loop.
	prompts []string
	// emitWikiWrite causes each loop to emit a wiki_write done event with id.
	ids []string
}

func (f *fakeRunner) run(ctx context.Context, _ *provider.Provider, _ string, _ []types.Tool, prompt types.Message, emit types.EventSink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.prompts = append(f.prompts, prompt.Text)
	for _, id := range f.ids {
		emit(types.Event{Type: "tool_execution_end", ToolName: "wiki_write", Args: types.ArgsMap{"id": id}})
	}
	return nil
}

func pt(id, name, summary string, tr [2]int, evidence []store.Evidence) store.InterestPoint {
	return store.InterestPoint{
		ID: id, Name: name, Summary: summary, TurnRange: tr, Status: "active",
		Reliability: store.Reliability{Confidence: 0.9, Status: "supported", Evidence: evidence},
		Freshness:   store.Freshness{Level: "fresh", UpdatedAt: time.Now()},
	}
}

func TestBuildPointPromptIncludesEvidenceDialogRelated(t *testing.T) {
	ip := pt("ip-1", "PostgreSQL", "团队默认数据库", [2]int{0, 2}, []store.Evidence{
		{Kind: "web", URL: "https://x.example", Query: "postgresql jsonb"},
	})
	dialog := "[USER]: 我们用 PostgreSQL\n[ASSISTANT]: 好的\n"
	related := "=== related: postgresql-page (score 0.90) ===\nID: postgresql-page\nPreview: 已有页内容..."

	prompt := buildPointPrompt(ip, dialog, related, "English")
	for _, want := range []string{"PostgreSQL", "https://x.example", "[USER]: 我们用 PostgreSQL", "postgresql-page"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}
}

func TestCompileRunsPerPointLoop(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	runner := &fakeRunner{ids: []string{"page-1"}}
	model := types.Model{ID: "m", BaseURL: "http://127.0.0.1:9/v1", API: provider.APIOpenAICompletions}
	w := NewWriter(deps, func(context.Context) (*provider.Provider, error) {
		return provider.NewConfiguredProvider(model, "test"), nil
	}, "English", true)
	w.runLoop = runner.run
	w.deps = ToolsDeps{Store: deps.Store, Vec: deps.Vec, Embedder: namedEmbedder{}}

	msgs := []types.Message{
		{Role: types.RoleUser, Text: "我们用 PostgreSQL"},
		{Role: types.RoleAssistant, Text: "好的"},
	}
	pts := []store.InterestPoint{
		pt("ip-1", "alpha", "a1", [2]int{0, 0}, nil),
		pt("ip-2", "beta", "b1", [2]int{0, 0}, nil),
	}
	touched, err := w.Compile(context.Background(), "agent-a", pts, msgs)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// alpha and beta are orthogonal under namedEmbedder → two isolated loops.
	if runner.calls != 2 {
		t.Errorf("loop calls = %d, want 2 (two isolated points, one loop each)", runner.calls)
	}
	if len(touched) != 2 || touched[0] != "page-1" || touched[1] != "page-1" {
		t.Errorf("touched = %v, want [page-1 page-1]", touched)
	}
}

func TestCompileGroupsClustersPerLoop(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	runner := &fakeRunner{}
	model := types.Model{ID: "m", BaseURL: "http://127.0.0.1:9/v1", API: provider.APIOpenAICompletions}
	w := NewWriter(deps, func(context.Context) (*provider.Provider, error) {
		return provider.NewConfiguredProvider(model, "test"), nil
	}, "English", true)
	w.runLoop = runner.run
	w.deps = ToolsDeps{Store: deps.Store, Vec: deps.Vec, Embedder: namedEmbedder{}}

	msgs := []types.Message{
		{Role: types.RoleUser, Text: "alpha"},
		{Role: types.RoleAssistant, Text: "ok"},
	}
	pts := []store.InterestPoint{
		pt("ip-a", "alpha", "a1", [2]int{0, 0}, nil),
		pt("ip-a2", "alpha2", "a2", [2]int{0, 0}, nil),
		pt("ip-b", "beta", "b1", [2]int{0, 0}, nil),
	}
	if _, err := w.Compile(context.Background(), "agent-a", pts, msgs); err != nil {
		t.Fatal(err)
	}
	// alpha+alpha2 share a vector → one cluster loop; beta is isolated → one
	// loop. Total 2 loops (not 3 per-point).
	if runner.calls != 2 {
		t.Errorf("loop calls = %d, want 2 (1 cluster + 1 isolated)", runner.calls)
	}
}

func TestBuildPointPromptIncludesEventTime(t *testing.T) {
	et := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ip := pt("ip-1", "PostgreSQL", "默认数据库", [2]int{0, 2}, nil)
	ip.EventTime = et
	prompt := buildPointPrompt(ip, "", "", "English")
	if !strings.Contains(prompt, "2026-08-01") {
		t.Errorf("prompt missing event time\n---\n%s", prompt)
	}
}

func TestCompileBackfillsEventTime(t *testing.T) {
	deps, st, _ := newTestDeps(t)
	ctx := context.Background()
	// Seed an existing page (update case) without EventTime.
	if err := st.UpsertPage(ctx, store.Page{ID: "page-1", AgentID: "agent-a", Title: "P1", BodyMD: "old", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{ids: []string{"page-1"}}
	model := types.Model{ID: "m", BaseURL: "http://127.0.0.1:9/v1", API: provider.APIOpenAICompletions}
	w := NewWriter(deps, func(context.Context) (*provider.Provider, error) {
		return provider.NewConfiguredProvider(model, "test"), nil
	}, "English", true)
	w.runLoop = runner.run

	et := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ip := pt("ip-1", "主题", "摘要", [2]int{0, 0}, nil)
	ip.EventTime = et
	if _, err := w.Compile(ctx, "agent-a", []store.InterestPoint{ip}, nil); err != nil {
		t.Fatal(err)
	}
	pg, err := st.GetPage(ctx, "agent-a", "page-1")
	if err != nil || pg == nil {
		t.Fatalf("page missing: %v", err)
	}
	if !pg.EventTime.Equal(et) {
		t.Errorf("page event_time = %v, want backfilled %v", pg.EventTime, et)
	}
}

func TestCompileDialogsSliceByTurnRange(t *testing.T) {
	msgs := []types.Message{
		{Role: types.RoleUser, Text: "u1"},
		{Role: types.RoleAssistant, Text: "a1"},
		{Role: types.RoleUser, Text: "u2"},
		{Role: types.RoleAssistant, Text: "a2"},
	}
	got := dialogSegment(msgs, [2]int{1, 2})
	if !strings.Contains(got, "u2") || strings.Contains(got, "u1") {
		t.Errorf("dialog segment = %q, want to start at u2 (global index 1..2)", got)
	}
}
