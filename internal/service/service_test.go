package service

import (
	"context"
	"testing"
	"time"

	"interest-memory/internal/config"
	"interest-memory/internal/fork"
	"interest-memory/internal/llm"
	"interest-memory/internal/recall"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/verify"
	"interest-memory/internal/wiki"

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

func TestPersistContradictionsLogsEdges(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &Service{store: st, verify: &fakeVerifier{cons: []store.Contradiction{
		{ID: "con1", LeftID: "c1", RightID: "c2", Description: "矛盾", Status: "open"},
	}}}
	pts := []store.InterestPoint{{ID: "ip1", Name: "A"}, {ID: "ip2", Name: "B"}}
	claims := []store.Claim{{ID: "c1", PageID: "ip1", Text: "x"}, {ID: "c2", PageID: "ip2", Text: "y"}}
	if err := svc.persistContradictions(context.Background(), "agent-a", pts, claims); err != nil {
		t.Fatal(err)
	}
	logs, err := st.ListLogs(context.Background(), "agent-a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0].Action != "edge_change" {
		t.Errorf("log action = %q, want edge_change", logs[0].Action)
	}
	if len(logs[0].Edges) != 2 || logs[0].Edges[0].Kind != store.EdgeContradict {
		t.Errorf("edges = %+v, want bidirectional contradicts", logs[0].Edges)
	}
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

// fakeVerifier implements verify.Verifier for the contradiction-log test.
type fakeVerifier struct{ cons []store.Contradiction }

func (f *fakeVerifier) VerifyCandidates(context.Context, string, []fork.Candidate) ([]verify.Verified, error) {
	return nil, nil
}
func (f *fakeVerifier) CheckClaims(context.Context, string, []store.InterestPoint) ([]store.Claim, error) {
	return nil, nil
}
func (f *fakeVerifier) FlagContradictions(context.Context, string, []store.Claim) ([]store.Contradiction, error) {
	return f.cons, nil
}
func (f *fakeVerifier) GradeForRecall(context.Context, string, []vec.Hit) ([]verify.Graded, error) {
	return nil, nil
}
func (f *fakeVerifier) FeedbackWrite(context.Context, string, []vec.Hit) error { return nil }

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

func TestServiceSearchPassthrough(t *testing.T) {
	want := []recall.Result{{Kind: "wiki_page", ID: "pg", Title: "T"}}
	svc := &Service{cfg: config.Config{Search: config.SearchConfig{TopK: 3, MaxBodyLen: 4000}}, recall: &fakeRecall{searchResults: want}}
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

type fakeCleaner struct {
	pts      []store.InterestPoint
	archived []string
	calls    int
}

func (f *fakeCleaner) Clean(context.Context, string, []verify.Verified) ([]store.InterestPoint, []string, error) {
	f.calls++
	return f.pts, f.archived, nil
}

type fakeWiki struct {
	touched    []string
	reconciles []wiki.ReconcileInput
}

func (f *fakeWiki) Compile(context.Context, string, []store.InterestPoint, []types.Message) ([]string, error) {
	return f.touched, nil
}
func (f *fakeWiki) RebuildEdges(context.Context, string) error { return nil }
func (f *fakeWiki) ReconcileRelated(_ context.Context, _ string, in wiki.ReconcileInput, _, _ int) error {
	f.reconciles = append(f.reconciles, in)
	return nil
}

// fakeDeleteVerifier reuses fakeVerifier but returns a delete-relation
// verified candidate so the pipeline archives without creating points.
type fakeDeleteVerifier struct{ fakeVerifier }

func (f *fakeDeleteVerifier) VerifyCandidates(context.Context, string, []fork.Candidate) ([]verify.Verified, error) {
	return []verify.Verified{{
		Candidate:    fork.Candidate{Topic: "old-topic"},
		Relation:     verify.RelationDelete,
		RelationToID: "ip-old",
	}}, nil
}

func TestProcessSessionDeleteOnlyStillReconciles(t *testing.T) {
	svc := &Service{
		fork:     &fakeFork{cands: []fork.Candidate{{Topic: "old-topic"}}},
		verify:   &fakeDeleteVerifier{},
		interest: &fakeCleaner{pts: nil, archived: []string{"ip-old"}},
		wiki:     &fakeWiki{},
	}
	err := svc.ProcessSession(context.Background(), "agent-a", store.Transcript{
		SessionID: "s1", AgentID: "agent-a", TurnCount: 1, RawTurns: `[{"role":"user","content":"x"}]`,
	})
	if err != nil {
		t.Fatalf("ProcessSession: %v", err)
	}
	fw := svc.wiki.(*fakeWiki)
	if len(fw.reconciles) == 0 {
		t.Fatal("ReconcileRelated was never called for a delete-only session")
	}
	got := fw.reconciles[len(fw.reconciles)-1]
	if len(got.ArchivedPoints) != 1 || got.ArchivedPoints[0] != "ip-old" {
		t.Errorf("archived points = %+v, want [ip-old]", got.ArchivedPoints)
	}
}

func TestProcessSessionNoOpReturnsEarly(t *testing.T) {
	svc := &Service{
		fork:     &fakeFork{cands: []fork.Candidate{{Topic: "t"}}},
		verify:   &fakeVerifier{},
		interest: &fakeCleaner{pts: nil, archived: nil},
		wiki:     &fakeWiki{},
	}
	err := svc.ProcessSession(context.Background(), "agent-a", store.Transcript{
		SessionID: "s1", AgentID: "agent-a", TurnCount: 1, RawTurns: `[{"role":"user","content":"x"}]`,
	})
	if err != nil {
		t.Fatalf("ProcessSession: %v", err)
	}
	fw := svc.wiki.(*fakeWiki)
	if len(fw.reconciles) != 0 {
		t.Errorf("reconcile called for no-op run: %+v", fw.reconciles)
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
