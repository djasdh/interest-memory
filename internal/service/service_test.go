package service

import (
	"context"
	"testing"
	"time"

	"interest-memory/internal/config"
	"interest-memory/internal/fork"
	"interest-memory/internal/recall"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/verify"
)

// fakeRecall implements recall.RecallService for passthrough tests.
type fakeRecall struct {
	searchResults []recall.Result
	byID          *recall.Result
}

func (f *fakeRecall) Recall(context.Context, string, string, recall.Options) (string, error) { return "", nil }
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
