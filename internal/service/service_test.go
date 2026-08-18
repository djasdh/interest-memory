package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/fork"
	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/recall"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/wiki"

	"github.com/djasdh/my-agent-core/types"
)

// fakeRecall implements recall.RecallService for passthrough tests.
type fakeRecall struct {
	searchResults []recall.Result
	byID          *recall.Result
}

func (f *fakeRecall) Recall(context.Context, string, string, recall.Options) (string, error) {
	return "", nil
}
func (f *fakeRecall) Search(_ context.Context, _, _ string, topK, maxBodyLen int) ([]recall.Result, error) {
	return f.searchResults, nil
}
func (f *fakeRecall) GetByID(_ context.Context, _, _ string, _ int) (*recall.Result, error) {
	return f.byID, nil
}

// captureRecall implements recall.RecallService for tests that need to
// observe the options passed through.
type captureRecall struct {
	opts    recall.Options
	results []recall.Result
	byID    *recall.Result
}

func (f *captureRecall) Recall(_ context.Context, _, _ string, opts recall.Options) (string, error) {
	f.opts = opts
	return "", nil
}
func (f *captureRecall) Search(_ context.Context, _, _ string, _, _ int) ([]recall.Result, error) {
	return f.results, nil
}
func (f *captureRecall) GetByID(_ context.Context, _, _ string, _ int) (*recall.Result, error) {
	return f.byID, nil
}

func TestServiceListLogsPassthrough(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.AppendLog(context.Background(), store.ChangeLog{ID: "l1", AgentID: "a", Action: "create"}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{store: st}
	got, err := svc.ListLogs(context.Background(), "a", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "l1" {
		t.Errorf("logs = %+v", got)
	}
}

func TestSetCandidateEventTime(t *testing.T) {
	svc := &Service{}
	now := time.Now().UTC()
	sd := now.Add(-24 * time.Hour)

	// session_date present → preferred.
	c1 := []fork.Candidate{{Topic: "a"}}
	svc.setCandidateEventTime(c1, &sd, now)
	if !c1[0].EventTime.Equal(sd) {
		t.Errorf("event_time = %v, want session_date %v", c1[0].EventTime, sd)
	}

	// session_date missing → received_at fallback.
	c2 := []fork.Candidate{{Topic: "b"}}
	svc.setCandidateEventTime(c2, nil, now)
	if !c2[0].EventTime.Equal(now) {
		t.Errorf("event_time = %v, want received_at %v", c2[0].EventTime, now)
	}
}

func TestRecallWikiDisabledForcesNoWiki(t *testing.T) {
	cr := &captureRecall{}
	svc := &Service{
		cfg:    config.Config{Wiki: config.WikiConfig{Enabled: false}, Recall: config.RecallConfig{TopK: 8, IncludeWiki: true, MinScore: 0.3}},
		recall: cr,
	}
	if _, err := svc.Recall(context.Background(), "a", "q", recall.Options{}); err != nil {
		t.Fatal(err)
	}
	if cr.opts.IncludeWiki {
		t.Error("IncludeWiki should be forced false when wiki disabled")
	}
}

func TestSearchWikiDisabledFiltersWikiPages(t *testing.T) {
	cr := &captureRecall{results: []recall.Result{
		{Kind: "interest_point", ID: "ip1", Title: "a"},
		{Kind: "wiki_page", ID: "pg1", Title: "b"},
	}}
	svc := &Service{cfg: config.Config{Wiki: config.WikiConfig{Enabled: false}}, recall: cr}
	got, err := svc.Search(context.Background(), "a", "q", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "interest_point" {
		t.Errorf("search = %+v, want only interest_point", got)
	}
}

func TestGetByIDWikiDisabledReturnsNilForWikiPage(t *testing.T) {
	cr := &captureRecall{byID: &recall.Result{ID: "pg1", Kind: "wiki_page", Title: "b"}}
	svc := &Service{cfg: config.Config{Wiki: config.WikiConfig{Enabled: false}}, recall: cr}
	got, err := svc.GetByID(context.Background(), "a", "pg1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("GetByID = %+v, want nil for wiki page when wiki disabled", got)
	}
}

func TestServiceSearchPassthrough(t *testing.T) {
	want := []recall.Result{{Kind: "wiki_page", ID: "pg", Title: "T"}}
	svc := &Service{cfg: config.Config{Wiki: config.WikiConfig{Enabled: true}, Search: config.SearchConfig{TopK: 3, MaxBodyLen: 4000}}, recall: &fakeRecall{searchResults: want}}
	got, err := svc.Search(context.Background(), "a", "q", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "pg" {
		t.Errorf("search = %+v", got)
	}
}

func TestServiceGetByIDPassthrough(t *testing.T) {
	svc := &Service{cfg: config.Config{Search: config.SearchConfig{MaxBodyLen: 4000}}, recall: &fakeRecall{byID: &recall.Result{ID: "ip-1", Kind: "interest_point"}}}
	got, err := svc.GetByID(context.Background(), "a", "ip-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "ip-1" {
		t.Errorf("by id = %+v", got)
	}
}

// TestProcessSessionEmptyTranscript: empty/no-interest transcript should
// return nil and not error (pipeline short-circuits).
func TestProcessSessionEmptyTranscript(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.LLM.BaseURL = "http://127.0.0.1:1/v1" // unreachable — must not be hit
	cfg.Verify.UseWebSearch = false

	// Build a service with a fake verifier chain? Simplest: use New with
	// a real store/vec and a nil-free embedder — but ProcessSession with
	// empty transcript returns before any LLM call.
	svc := New(cfg, st, nil, nil, nil)
	err = svc.ProcessSession(context.Background(), "agent-a", store.Transcript{
		SessionID: "s1", AgentID: "agent-a", TurnCount: 0, RawTurns: "[]",
	})
	if err != nil {
		t.Fatalf("ProcessSession(empty) error: %v", err)
	}
}

// TestProcessSessionMalformedTranscript: unparseable raw turns surfaces error.
func TestProcessSessionMalformedTranscript(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Verify.UseWebSearch = false
	svc := New(cfg, st, nil, nil, nil)
	err = svc.ProcessSession(context.Background(), "agent-a", store.Transcript{
		SessionID: "s1", AgentID: "agent-a", TurnCount: 1, RawTurns: "{not json",
	})
	if err == nil {
		t.Fatal("expected error for malformed transcript")
	}
}

func TestStatsCounts(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	ip := store.InterestPoint{ID: "ip1", AgentID: "agent-a", Name: "x", Status: "active",
		FirstSeenAt: now, LastSeenAt: now}
	if err := st.UpsertInterestPoint(ctx, ip); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	svc := New(cfg, st, nil, nil, nil)
	stats, err := svc.Stats(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if stats["interest_points"] != 1 {
		t.Errorf("interest_points = %d, want 1", stats["interest_points"])
	}
	if stats["wiki_pages"] != 0 {
		t.Errorf("wiki_pages = %d, want 0", stats["wiki_pages"])
	}
}

func TestForkManualReturnsOldestUnprocessed(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.SaveTranscript(ctx, store.Transcript{SessionID: "s1", AgentID: "agent-a", TurnCount: 2, RawTurns: "[]"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTranscript(ctx, store.Transcript{SessionID: "s2", AgentID: "agent-a", TurnCount: 1, RawTurns: "[]"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	svc := New(cfg, st, nil, nil, nil)
	tx, err := svc.ForkManual(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if tx == nil || tx.SessionID != "s1" {
		t.Fatalf("fork manual = %+v, want s1 (oldest)", tx)
	}
	// None unprocessed → nil.
	if err := st.MarkTranscriptProcessed(ctx, "agent-a", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTranscriptProcessed(ctx, "agent-a", "s2"); err != nil {
		t.Fatal(err)
	}
	tx, err = svc.ForkManual(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if tx != nil {
		t.Fatalf("expected nil when nothing unprocessed, got %+v", tx)
	}
}

// ---- fake pipeline stages for ProcessSession tests ----

type fakeFork struct{ cands []fork.Candidate }

func (f *fakeFork) Analyze(context.Context, string, [][]llm.Message) ([]fork.Candidate, error) {
	return f.cands, nil
}

type fakeWiki struct {
	touched    []string
	reconciles []wiki.ReconcileInput
	compiles   int
	compiled   []store.InterestPoint
}

func (f *fakeWiki) Compile(_ context.Context, _ string, pts []store.InterestPoint, _ []types.Message) ([]string, error) {
	f.compiles++
	f.compiled = pts
	return f.touched, nil
}
func (f *fakeWiki) RebuildEdges(context.Context, string, []string) error { return nil }
func (f *fakeWiki) ReconcileRelated(_ context.Context, _ string, in wiki.ReconcileInput, _, _ int) error {
	f.reconciles = append(f.reconciles, in)
	return nil
}

// fakePipelineEmbedder implements interest.Embedder for the V1 pipeline.
// Returns a constant vector so single-candidate sessions cluster as isolated
// points (no LLM merge call) and land in the adjudication stage.
type fakePipelineEmbedder struct{}

func (fakePipelineEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

// fakePipelineLLM implements interest.ClusterLLM. Isolated-point calls expect
// a meta verdict; component calls are not exercised in these tests.
type fakePipelineLLM struct {
	meta map[string]any
}

func (f *fakePipelineLLM) ChatJSON(_ context.Context, _ []llm.Message, out any) error {
	b, err := json.Marshal(map[string]any{"meta": f.meta})
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// newTestService builds a Service wired for the V1 pipeline with a real
// store/vec and fake embedder/llm. Wiki is the caller's fake.
func newTestService(t *testing.T, cfg config.Config, fw *fakeWiki) *Service {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vi, err := vec.NewSQLiteVec(st.DB(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !vi.Available() {
		t.Skip("sqlite-vec not available")
	}
	svc := &Service{
		cfg:      cfg,
		store:    st,
		fork:     &fakeFork{cands: []fork.Candidate{{Topic: "t", Confidence: 0.9}}},
		wiki:     fw,
		embedder: fakePipelineEmbedder{},
		llm:      &fakePipelineLLM{meta: map[string]any{"wiki_worthy": true}},
		vec:      vi,
	}
	return svc
}

func TestProcessSessionWikiDisabledSkipsWikiStage(t *testing.T) {
	fw := &fakeWiki{}
	svc := newTestService(t, config.Config{Wiki: config.WikiConfig{Enabled: false}}, fw)
	err := svc.ProcessSession(context.Background(), "agent-a", store.Transcript{
		SessionID: "s1", AgentID: "agent-a", TurnCount: 1, RawTurns: `[{"role":"user","content":"x"}]`,
	})
	if err != nil {
		t.Fatalf("ProcessSession: %v", err)
	}
	if fw.compiles != 0 {
		t.Errorf("Compile called %d times, want 0 when wiki disabled", fw.compiles)
	}
	if len(fw.reconciles) != 0 {
		t.Errorf("ReconcileRelated called %d times, want 0 when wiki disabled", len(fw.reconciles))
	}
}

func TestProcessSessionPipelinePersistsAndCompiles(t *testing.T) {
	fw := &fakeWiki{}
	svc := newTestService(t, config.Default(), fw)
	err := svc.ProcessSession(context.Background(), "agent-a", store.Transcript{
		SessionID: "s1", AgentID: "agent-a", TurnCount: 1, RawTurns: `[{"role":"user","content":"x"}]`,
	})
	if err != nil {
		t.Fatalf("ProcessSession: %v", err)
	}
	// The candidate persisted as an interest point (isolated → create).
	pts, err := svc.store.ListInterestPoints(context.Background(), "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("persisted points = %d, want 1", len(pts))
	}
	// Wiki stage received the final point and compiled it.
	if fw.compiles != 1 {
		t.Errorf("Compile called %d times, want 1", fw.compiles)
	}
	if len(fw.compiled) != 1 || fw.compiled[0].ID != pts[0].ID {
		t.Errorf("compiled = %+v, want the persisted point", fw.compiled)
	}
	// Reconcile ran with no archived points.
	if len(fw.reconciles) != 1 {
		t.Fatalf("ReconcileRelated called %d times, want 1", len(fw.reconciles))
	}
	if len(fw.reconciles[0].ArchivedPoints) != 0 {
		t.Errorf("archived points = %+v, want none", fw.reconciles[0].ArchivedPoints)
	}
}

func TestProcessSessionNoEmbedderShortCircuits(t *testing.T) {
	// embedder nil → V1 pipeline skipped, nothing persisted, wiki not called.
	fw := &fakeWiki{}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &Service{
		cfg:   config.Default(),
		store: st,
		fork:  &fakeFork{cands: []fork.Candidate{{Topic: "t", Confidence: 0.9}}},
		wiki:  fw,
	}
	err = svc.ProcessSession(context.Background(), "agent-a", store.Transcript{
		SessionID: "s1", AgentID: "agent-a", TurnCount: 1, RawTurns: `[{"role":"user","content":"x"}]`,
	})
	if err != nil {
		t.Fatalf("ProcessSession: %v", err)
	}
	if fw.compiles != 0 {
		t.Errorf("Compile called %d times, want 0 (no embedder)", fw.compiles)
	}
	pts, _ := svc.store.ListInterestPoints(context.Background(), "agent-a")
	if len(pts) != 0 {
		t.Errorf("persisted points = %d, want 0 (no embedder)", len(pts))
	}
}

func TestBuildNamespaceResolverModes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mk := func(id, agent string) store.InterestPoint {
		return store.InterestPoint{ID: id, AgentID: agent, Name: "n-" + id, Status: "active"}
	}
	if err := st.UpsertInterestPoint(ctx, mk("a1", "agent-a")); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertInterestPoint(ctx, mk("b1", "agent-b")); err != nil {
		t.Fatal(err)
	}

	// isolated (default) → nil resolver (each agent reads only itself).
	cfg := config.Default()
	if r := buildNamespaceResolver(cfg, st); r != nil {
		t.Error("isolated mode should return a nil resolver")
	}

	// all → dynamically discovers every persisted namespace.
	cfg.Namespaces.Mode = config.NamespaceAll
	r := buildNamespaceResolver(cfg, st)
	ns, err := r(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range ns {
		got[n] = true
	}
	if !got["agent-a"] || !got["agent-b"] {
		t.Errorf("all-mode namespaces = %v, want both agent-a and agent-b", ns)
	}

	// custom → one-way visible_to map (no store discovery).
	cfg = config.Default()
	cfg.Namespaces.Mode = config.NamespaceCustom
	cfg.Namespaces.VisibleTo = map[string][]string{"agent-a": {"agent-b"}}
	r = buildNamespaceResolver(cfg, st)
	ns, err = r(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 || ns[0] != "agent-b" {
		t.Errorf("custom visible_to[agent-a] = %v, want [agent-b]", ns)
	}
	// Unconfigured agent → empty visible set (reads only itself via recall).
	ns, err = r(ctx, "agent-c")
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 0 {
		t.Errorf("unconfigured agent visible_to = %v, want empty", ns)
	}
}

func TestListGraphAssemblesNodesAndEdges(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()

	if err := st.UpsertInterestPoint(ctx, store.InterestPoint{
		ID: "ip1", AgentID: "a", Name: "Go", Status: "active", Importance: 0.9,
		Keywords: []string{"lang"}, FirstSeenAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPage(ctx, store.Page{
		ID: "pg1", AgentID: "a", PageType: store.PageConcept, Title: "Go 并发",
		Status: "active", Tags: []string{"go"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPage(ctx, store.Page{
		ID: "pg2", AgentID: "a", PageType: store.PageSource, Title: "已归档页",
		Status: "archived", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// has_page (ip→page), related (page→page), contradicts (auto reverse), and
	// a dangling edge to an id with no node (must be dropped).
	if err := st.AddEdgePairs(ctx, "a", []store.Edge{
		{SourceID: "ip1", TargetID: "pg1", Kind: store.EdgeHasPage, Weight: 1},
		{SourceID: "pg1", TargetID: "pg2", Kind: store.EdgeRelated, Weight: 0.5},
		{SourceID: "pg1", TargetID: "pg2", Kind: store.EdgeContradict, Weight: 1},
		{SourceID: "pg1", TargetID: "ghost", Kind: store.EdgeRelated, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}

	svc := &Service{store: st}
	g, err := svc.ListGraph(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3: %+v", len(g.Nodes), g.Nodes)
	}
	// Medium fields only: no summary/body leaked into the payload.
	for _, n := range g.Nodes {
		if n.Kind == "interest_point" && n.Importance != 0.9 {
			t.Errorf("node %s importance = %v, want 0.9", n.ID, n.Importance)
		}
	}
	// 4 stored edges + 1 reverse contradicts − 1 dangling = 4.
	if len(g.Edges) != 4 {
		t.Fatalf("edges = %d, want 4: %+v", len(g.Edges), g.Edges)
	}
	kinds := map[store.EdgeType]int{}
	for _, e := range g.Edges {
		kinds[e.Kind]++
		if e.Source == "ghost" || e.Target == "ghost" {
			t.Errorf("dangling edge survived: %+v", e)
		}
	}
	if kinds[store.EdgeHasPage] != 1 || kinds[store.EdgeRelated] != 1 || kinds[store.EdgeContradict] != 2 {
		t.Errorf("kind counts = %+v, want has_page=1 related=1 contradict=2", kinds)
	}
}

func TestListGraphIdCollisionPrefixes(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()

	if err := st.UpsertInterestPoint(ctx, store.InterestPoint{
		ID: "same", AgentID: "a", Name: "同名兴趣点", Status: "active",
		FirstSeenAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPage(ctx, store.Page{
		ID: "same", AgentID: "a", PageType: store.PageConcept, Title: "同名页面",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Collided id as edge endpoint is ambiguous → dropped.
	if err := st.AddEdgePair(ctx, "a", store.Edge{SourceID: "same", TargetID: "same", Kind: store.EdgeRelated, Weight: 1}); err != nil {
		t.Fatal(err)
	}

	svc := &Service{store: st}
	g, err := svc.ListGraph(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (prefixed): %+v", len(g.Nodes), g.Nodes)
	}
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		ids[n.ID] = true
	}
	if !ids["interest_point:same"] || !ids["wiki_page:same"] {
		t.Errorf("collided nodes not prefixed: %v", ids)
	}
	if len(g.Edges) != 0 {
		t.Errorf("ambiguous collided edge should be dropped, got %+v", g.Edges)
	}
}

func TestNewWiresGroupSimIntoWriter(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Wiki.GroupSim = 0.4
	cfg.Wiki.Enabled = false // avoid provider calls; only wiring is under test
	svc := New(cfg, st, nil, nil, nil)
	if svc == nil {
		t.Fatal("nil service")
	}
	w, ok := svc.wiki.(*wiki.Writer)
	if !ok {
		t.Fatalf("wiki is %T, want *wiki.Writer", svc.wiki)
	}
	if w.GroupSim() != 0.4 {
		t.Errorf("writer groupSim = %v, want 0.4 (wired from config)", w.GroupSim())
	}
}
