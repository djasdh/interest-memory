package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInterestPointCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	p := InterestPoint{
		ID: "ip-1", AgentID: "agent-a", Name: "分布式系统",
		Summary: "最终一致性设计", Keywords: []string{"distributed", "consensus"},
		Importance: 0.8, Status: "active",
		Reliability: Reliability{Confidence: 0.9, Status: "supported",
			Evidence: []Evidence{{Kind: "session", SourceID: "s-1"}}},
		Freshness:   Freshness{Level: "fresh", UpdatedAt: now, TTLDays: 30},
		FirstSeenAt: now, LastSeenAt: now, SeenCount: 1,
		SourceSessions: []string{"s-1"},
	}
	if err := s.UpsertInterestPoint(ctx, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.GetInterestPoint(ctx, "agent-a", "ip-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Name != "分布式系统" || got.Importance != 0.8 {
		t.Fatalf("Get mismatch: %+v", got)
	}
	if len(got.Keywords) != 2 || got.Keywords[0] != "distributed" {
		t.Errorf("keywords = %v", got.Keywords)
	}
	if len(got.Reliability.Evidence) != 1 || got.Reliability.Evidence[0].Kind != "session" {
		t.Errorf("evidence = %v", got.Reliability.Evidence)
	}
	// Agent isolation
	other, _ := s.GetInterestPoint(ctx, "agent-b", "ip-1")
	if other != nil {
		t.Error("cross-agent leak: agent-b should not see agent-a's point")
	}
	// Keyword search
	hits, err := s.SearchInterestPointsByKeywords(ctx, "agent-a", "分布式", 5)
	if err != nil || len(hits) != 1 {
		t.Errorf("keyword search hits = %d, err=%v", len(hits), err)
	}
}

func TestPageAndClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	p := Page{ID: "pg-1", AgentID: "agent-a", PageType: PageConcept,
		Title: "CAP 定理", BodyMD: "# CAP\n[[分布式系统]] 说明", CreatedAt: now, UpdatedAt: now}
	if err := s.UpsertPage(ctx, p); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	got, err := s.GetPage(ctx, "agent-a", "pg-1")
	if err != nil || got == nil {
		t.Fatalf("GetPage: %v %v", got, err)
	}
	c := Claim{ID: "cl-1", AgentID: "agent-a", PageID: "pg-1", Text: "CAP 中 P 是分区容忍",
		Status: "supported", Confidence: 0.95,
		Evidence:  []Evidence{{Kind: "page", SourceID: "pg-2"}},
		Freshness: Freshness{Level: "fresh", UpdatedAt: now, TTLDays: 30}}
	if err := s.UpsertClaim(ctx, c); err != nil {
		t.Fatalf("UpsertClaim: %v", err)
	}
	claims, err := s.ListClaims(ctx, "agent-a", "pg-1")
	if err != nil || len(claims) != 1 || claims[0].Text != c.Text {
		t.Errorf("claims = %+v err=%v", claims, err)
	}
}

func TestEdgeAndEnsureContradictPair(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Set up two pages
	now := time.Now()
	s.UpsertPage(ctx, Page{ID: "a", AgentID: "ag", PageType: PageConcept, Title: "A", CreatedAt: now, UpdatedAt: now})
	s.UpsertPage(ctx, Page{ID: "b", AgentID: "ag", PageType: PageConcept, Title: "B", CreatedAt: now, UpdatedAt: now})

	// Related edge (single direction)
	if err := s.AddEdgePair(ctx, "ag", Edge{SourceID: "a", TargetID: "b", Kind: EdgeRelated, Weight: 0.8}); err != nil {
		t.Fatalf("AddEdgePair related: %v", err)
	}
	outs, _ := s.Outlinks(ctx, "ag", "a")
	if len(outs) != 1 || outs[0].Kind != EdgeRelated {
		t.Errorf("outlinks = %+v", outs)
	}

	// Contradict edge must create BOTH directions
	if err := s.AddEdgePair(ctx, "ag", Edge{SourceID: "a", TargetID: "b", Kind: EdgeContradict, Weight: 0.9}); err != nil {
		t.Fatalf("AddEdgePair contradict: %v", err)
	}
	outs, _ = s.Outlinks(ctx, "ag", "a")
	backs, _ := s.Backlinks(ctx, "ag", "b")
	var hasFwd, hasRev bool
	for _, e := range outs {
		if e.TargetID == "b" && e.Kind == EdgeContradict {
			hasFwd = true
		}
	}
	for _, e := range backs {
		if e.SourceID == "a" && e.Kind == EdgeContradict {
			hasRev = true
		}
	}
	if !hasFwd || !hasRev {
		t.Errorf("contradict pair not enforced: fwd=%v rev=%v (outs=%+v backs=%+v)", hasFwd, hasRev, outs, backs)
	}
	// Backlinks of b should include both related(from a) and contradicts(from a)
	if len(backs) < 2 {
		t.Errorf("backlinks len = %d, want >= 2", len(backs))
	}

	// Delete edges for a
	if err := s.DeleteEdgesFor(ctx, "ag", "a"); err != nil {
		t.Fatalf("DeleteEdgesFor: %v", err)
	}
	outs, _ = s.Outlinks(ctx, "ag", "a")
	if len(outs) != 0 {
		t.Errorf("after delete outlinks = %+v, want empty", outs)
	}
}

func TestConcurrentWrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			now := time.Now()
			err := s.UpsertInterestPoint(ctx, InterestPoint{
				ID: fmt.Sprintf("ip-%d", n), AgentID: "agent-a", Name: fmt.Sprintf("点%d", n),
				Importance: float64(n) / 20, Status: "active",
				FirstSeenAt: now, LastSeenAt: now,
			})
			if err != nil {
				t.Errorf("concurrent upsert %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
	pts, err := s.ListInterestPoints(ctx, "agent-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pts) != 20 {
		t.Errorf("listed %d points, want 20", len(pts))
	}
}

func TestTranscript(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tx := Transcript{SessionID: "s-1", AgentID: "ag", TurnCount: 5,
		RawTurns: `[{"role":"user","content":"hi"}]`, ReceivedAt: time.Now()}
	if err := s.SaveTranscript(ctx, tx); err != nil {
		t.Fatalf("SaveTranscript: %v", err)
	}
	got, err := s.GetTranscript(ctx, "ag", "s-1")
	if err != nil || got == nil || got.TurnCount != 5 {
		t.Fatalf("GetTranscript: %+v err=%v", got, err)
	}
	if err := s.MarkTranscriptProcessed(ctx, "ag", "s-1"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	got2, _ := s.GetTranscript(ctx, "ag", "s-1")
	if got2.ProcessedAt == nil {
		t.Error("processed_at not set")
	}
}

func TestListUnprocessedTranscripts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	t1 := Transcript{SessionID: "s-1", AgentID: "ag", TurnCount: 2, RawTurns: "[]", ReceivedAt: now.Add(-2 * time.Minute)}
	t2 := Transcript{SessionID: "s-2", AgentID: "ag", TurnCount: 3, RawTurns: "[]", ReceivedAt: now.Add(-1 * time.Minute)}
	if err := s.SaveTranscript(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTranscript(ctx, t2); err != nil {
		t.Fatal(err)
	}
	// Another agent's transcript should not leak in.
	if err := s.SaveTranscript(ctx, Transcript{SessionID: "s-3", AgentID: "other", TurnCount: 1, RawTurns: "[]"}); err != nil {
		t.Fatal(err)
	}

	// Both unprocessed, oldest first.
	list, err := s.ListUnprocessedTranscripts(ctx, "ag")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].SessionID != "s-1" || list[1].SessionID != "s-2" {
		t.Fatalf("unprocessed = %+v", list)
	}

	// After marking one processed, only the other remains.
	if err := s.MarkTranscriptProcessed(ctx, "ag", "s-1"); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListUnprocessedTranscripts(ctx, "ag")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SessionID != "s-2" {
		t.Fatalf("unprocessed after mark = %+v", list)
	}
}
