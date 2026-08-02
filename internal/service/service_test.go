package service

import (
	"context"
	"testing"
	"time"

	"interest-memory/internal/config"
	"interest-memory/internal/store"
)

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
