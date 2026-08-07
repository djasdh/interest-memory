package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestInterestPointSubjectiveAndEvidenceLocators(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	p := InterestPoint{
		ID: "ip-sub", AgentID: "a", Name: "偏好", Summary: "喜欢 Go",
		Status:     "active",
		Subjective: true,
		Reliability: Reliability{
			Confidence: 0.9, Status: "supported",
			Evidence: []Evidence{{
				Kind: "web", SourceID: "u", URL: "https://x.example",
				TurnRange: [2]int{2, 3}, Query: "go lang", CapturedAt: now, Excerpt: "e",
			}},
		},
		Freshness: Freshness{Level: "fresh", UpdatedAt: now},
	}
	if err := s.UpsertInterestPoint(ctx, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.GetInterestPoint(ctx, "a", "ip-sub")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if !got.Subjective {
		t.Error("subjective not persisted (want true)")
	}
	ev := got.Reliability.Evidence
	if len(ev) != 1 {
		t.Fatalf("evidence = %d, want 1", len(ev))
	}
	if ev[0].URL != "https://x.example" || ev[0].Query != "go lang" || ev[0].TurnRange != [2]int{2, 3} {
		t.Errorf("evidence locators = %+v", ev[0])
	}
	if ev[0].CapturedAt.IsZero() {
		t.Error("captured_at is zero")
	}
}

func TestInterestPointTurnRangePersisted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	p := InterestPoint{
		ID: "ip-tr", AgentID: "a", Name: "点", Summary: "s",
		Status:      "active",
		TurnRange:   [2]int{3, 9},
		Reliability: Reliability{Confidence: 0.8, Status: "supported"},
		Freshness:   Freshness{Level: "fresh", UpdatedAt: now},
	}
	if err := s.UpsertInterestPoint(ctx, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.GetInterestPoint(ctx, "a", "ip-tr")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.TurnRange != [2]int{3, 9} {
		t.Errorf("turn_range = %v, want [3 9]", got.TurnRange)
	}
}

func TestPageStatusPersisted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.UpsertPage(ctx, Page{ID: "pg-a", AgentID: "a", Title: "t", BodyMD: "b", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	got, err := s.GetPage(ctx, "a", "pg-a")
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if got == nil || got.Status != "active" {
		t.Errorf("default status = %q, want active", got.Status)
	}

	got.Status = "superseded"
	if err := s.UpsertPage(ctx, *got); err != nil {
		t.Fatalf("UpsertPage superseded: %v", err)
	}
	got2, err := s.GetPage(ctx, "a", "pg-a")
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != "superseded" {
		t.Errorf("status = %q, want superseded", got2.Status)
	}
}

func TestChangeLogAppendAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	logs := []ChangeLog{
		{ID: "l1", AgentID: "a", EntityKind: "wiki_page", EntityID: "pg1", Title: "Page1", Action: "create",
			Edges: []LogEdge{{Action: "add", SourceID: "ip1", TargetID: "pg1", Kind: EdgeHasPage, Weight: 1}}},
		{ID: "l2", AgentID: "a", EntityKind: "interest_point", EntityID: "ip1", Title: "点1", Action: "archive"},
	}
	for _, l := range logs {
		if err := s.AppendLog(ctx, l); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}
	got, err := s.ListLogs(ctx, "a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Descending: newest first
	if len(got) != 2 || got[0].ID != "l2" || got[1].ID != "l1" {
		t.Errorf("logs order = %+v, want [l2 l1]", idsOfLogs(got))
	}
	if len(got[1].Edges) != 1 || got[1].Edges[0].Kind != EdgeHasPage {
		t.Errorf("edges = %+v", got[1].Edges)
	}
	// Agent isolation
	other, _ := s.ListLogs(ctx, "b", 0, 0)
	if len(other) != 0 {
		t.Errorf("agent-b logs = %d, want 0", len(other))
	}
}

func TestChangeLogListPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.AppendLog(ctx, ChangeLog{ID: fmt.Sprintf("l%d", i), AgentID: "a", EntityID: "e", Title: "t", Action: "update"}); err != nil {
			t.Fatal(err)
		}
	}
	// limit=2, offset=1 → descending [l3 l2]
	got, err := s.ListLogs(ctx, "a", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "l3" || got[1].ID != "l2" {
		t.Errorf("paged = %+v, want [l3 l2]", idsOfLogs(got))
	}
}

func TestChangeLogRetainCaps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.AppendLog(ctx, ChangeLog{ID: fmt.Sprintf("l%d", i), AgentID: "a", EntityID: "e", Title: "t", Action: "update"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetLogRetain(ctx, "a", 3); err != nil {
		t.Fatal(err)
	}
	// Append another entry to trigger cleanup → only the latest 3 are kept
	if err := s.AppendLog(ctx, ChangeLog{ID: "l5", AgentID: "a", EntityID: "e", Title: "t", Action: "update"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListLogs(ctx, "a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("logs after retain = %d, want 3", len(got))
	}
	if got[0].ID != "l5" {
		t.Errorf("newest = %s, want l5", got[0].ID)
	}
}

func TestChangeLogRetainDefault(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Clear any per-agent cap left by earlier tests (global map).
	_ = s.SetLogRetain(ctx, "a", 0)
	if err := s.SetLogRetainDefault(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := s.AppendLog(ctx, ChangeLog{ID: fmt.Sprintf("d%d", i), AgentID: "a", EntityID: "e", Title: "t", Action: "update"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListLogs(ctx, "a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("logs = %d, want 2 (default retain)", len(got))
	}
}

func idsOfLogs(logs []ChangeLog) []string {
	out := make([]string, len(logs))
	for i, l := range logs {
		out[i] = l.ID
	}
	return out
}

func TestPageTagsAndSourcesPersisted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	p := Page{ID: "pg-tag", AgentID: "a", Title: "T", BodyMD: "b", Status: "active",
		Tags:      []string{"go", "database"},
		Sources:   []string{"https://x.example", "postgresql-page"},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.UpsertPage(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPage(ctx, "a", "pg-tag")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Tags) != 2 || got.Tags[0] != "go" || len(got.Sources) != 2 || got.Sources[1] != "postgresql-page" {
		t.Errorf("tags/sources = %+v / %+v", got.Tags, got.Sources)
	}
}

func TestListTagsAggregates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	pages := []Page{
		{ID: "p1", AgentID: "a", Title: "1", Tags: []string{"go", "database"}, CreatedAt: now, UpdatedAt: now},
		{ID: "p2", AgentID: "a", Title: "2", Tags: []string{"go", "backend"}, CreatedAt: now, UpdatedAt: now},
		{ID: "p3", AgentID: "b", Title: "3", Tags: []string{"go"}, CreatedAt: now, UpdatedAt: now},
	}
	for _, p := range pages {
		if err := s.UpsertPage(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	tags, err := s.ListTags(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, tc := range tags {
		counts[tc.Tag] = tc.Count
	}
	if counts["go"] != 2 || counts["database"] != 1 || counts["backend"] != 1 {
		t.Errorf("agent-a tags = %v, want go:2 database:1 backend:1", counts)
	}
	if len(tags) != 3 {
		t.Errorf("tag count = %d, want 3 (deduped)", len(tags))
	}
	// Count descending: go first.
	if tags[0].Tag != "go" || tags[0].Count != 2 {
		t.Errorf("tags order = %+v, want go first", tags)
	}
	// Agent isolation.
	bt, _ := s.ListTags(ctx, "b")
	if len(bt) != 1 || bt[0].Tag != "go" {
		t.Errorf("agent-b tags = %+v", bt)
	}
}

func TestTranscriptSessionDateRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sd := now.Add(-24 * time.Hour)

	// With session_date set.
	if err := s.SaveTranscript(ctx, Transcript{SessionID: "s1", AgentID: "a", TurnCount: 2, RawTurns: "[]", ReceivedAt: now, SessionDate: &sd}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTranscript(ctx, "a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.SessionDate == nil || !got.SessionDate.Equal(sd) {
		t.Errorf("session_date = %v, want %v", got.SessionDate, sd)
	}
	if !got.ReceivedAt.Equal(now) {
		t.Errorf("received_at = %v, want %v (unchanged)", got.ReceivedAt, now)
	}

	// Without session_date (nil).
	if err := s.SaveTranscript(ctx, Transcript{SessionID: "s2", AgentID: "a", TurnCount: 1, RawTurns: "[]", ReceivedAt: now}); err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetTranscript(ctx, "a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if got2.SessionDate != nil {
		t.Errorf("session_date = %v, want nil", got2.SessionDate)
	}
}

func TestEventTimeRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	et := now.Add(-48 * time.Hour)

	p := InterestPoint{ID: "ip-et", AgentID: "a", Name: "点", Summary: "s", Status: "active",
		EventTime: et, Reliability: Reliability{Confidence: 0.8}, Freshness: Freshness{Level: "fresh", UpdatedAt: now}}
	if err := s.UpsertInterestPoint(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInterestPoint(ctx, "a", "ip-et")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.EventTime.Equal(et) {
		t.Errorf("interest event_time = %v, want %v", got.EventTime, et)
	}

	pg := Page{ID: "pg-et", AgentID: "a", Title: "T", BodyMD: "b", EventTime: et}
	if err := s.UpsertPage(ctx, pg); err != nil {
		t.Fatal(err)
	}
	pgGot, err := s.GetPage(ctx, "a", "pg-et")
	if err != nil {
		t.Fatal(err)
	}
	if pgGot == nil || !pgGot.EventTime.Equal(et) {
		t.Errorf("page event_time = %v, want %v", pgGot.EventTime, et)
	}
}

func TestResolveReplacementWalksSequelChain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// Chain: ip-old(archived) --sequel--> ip-new(active)
	s.UpsertInterestPoint(ctx, InterestPoint{ID: "ip-old", AgentID: "a", Name: "旧", Status: "archived"})
	s.UpsertInterestPoint(ctx, InterestPoint{ID: "ip-new", AgentID: "a", Name: "新", Status: "active"})
	// ip-new has a concept page.
	s.UpsertPage(ctx, Page{ID: "pg-new", AgentID: "a", Title: "新页", Status: "active", CreatedAt: now, UpdatedAt: now})
	s.AddEdgePair(ctx, "a", Edge{SourceID: "ip-old", TargetID: "ip-new", Kind: EdgeSequel, Weight: 1})
	s.AddEdgePair(ctx, "a", Edge{SourceID: "ip-new", TargetID: "pg-new", Kind: EdgeHasPage, Weight: 1})

	rep, err := s.ResolveReplacement(ctx, "a", "ip-old")
	if err != nil {
		t.Fatal(err)
	}
	if rep == nil {
		t.Fatal("no replacement resolved")
	}
	if rep.InterestPointID != "ip-new" {
		t.Errorf("replacement ip = %q, want ip-new", rep.InterestPointID)
	}
	if rep.Page == nil || rep.Page.ID != "pg-new" {
		t.Errorf("replacement page = %+v, want pg-new", rep.Page)
	}
}

func TestResolveReplacementPageIDResolvesViaHasPage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// Old point archived, its concept page old-page, sequel to a live point
	// with its own page.
	s.UpsertInterestPoint(ctx, InterestPoint{ID: "ip-old", AgentID: "a", Name: "旧", Status: "archived"})
	s.UpsertInterestPoint(ctx, InterestPoint{ID: "ip-new", AgentID: "a", Name: "新", Status: "active"})
	s.UpsertPage(ctx, Page{ID: "pg-old", AgentID: "a", Title: "旧页", Status: "superseded", CreatedAt: now, UpdatedAt: now})
	s.UpsertPage(ctx, Page{ID: "pg-new", AgentID: "a", Title: "新页", Status: "active", CreatedAt: now, UpdatedAt: now})
	s.AddEdgePair(ctx, "a", Edge{SourceID: "ip-old", TargetID: "pg-old", Kind: EdgeHasPage, Weight: 1})
	s.AddEdgePair(ctx, "a", Edge{SourceID: "ip-old", TargetID: "ip-new", Kind: EdgeSequel, Weight: 1})
	s.AddEdgePair(ctx, "a", Edge{SourceID: "ip-new", TargetID: "pg-new", Kind: EdgeHasPage, Weight: 1})

	rep, err := s.ResolveReplacement(ctx, "a", "pg-old") // query by page id
	if err != nil {
		t.Fatal(err)
	}
	if rep == nil || rep.Page == nil || rep.Page.ID != "pg-new" {
		t.Errorf("replacement from page id = %+v, want pg-new", rep)
	}
}

func TestResolveReplacementNoneForActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertInterestPoint(ctx, InterestPoint{ID: "ip-active", AgentID: "a", Name: "活跃", Status: "active"})

	rep, err := s.ResolveReplacement(ctx, "a", "ip-active")
	if err != nil {
		t.Fatal(err)
	}
	if rep != nil {
		t.Errorf("active entity should have no replacement, got %+v", rep)
	}
}

func TestResolveReplacementNoneWhenNoSequel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertInterestPoint(ctx, InterestPoint{ID: "ip-del", AgentID: "a", Name: "已删", Status: "archived"})

	rep, err := s.ResolveReplacement(ctx, "a", "ip-del")
	if err != nil {
		t.Fatal(err)
	}
	if rep != nil {
		t.Errorf("deleted entity should have no replacement, got %+v", rep)
	}
}

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

func TestListAgentIDsDistinct(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// No data → empty.
	ids, err := s.ListAgentIDs(ctx)
	if err != nil {
		t.Fatalf("ListAgentIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("empty store agent ids = %v, want []", ids)
	}

	mkIP := func(id, agent string) InterestPoint {
		return InterestPoint{ID: id, AgentID: agent, Name: "n-" + id, Status: "active"}
	}
	mkPage := func(id, agent string) Page {
		return Page{ID: id, AgentID: agent, Title: "t-" + id, Status: "active"}
	}
	if err := s.UpsertInterestPoint(ctx, mkIP("a1", "agent-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertInterestPoint(ctx, mkIP("a2", "agent-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertInterestPoint(ctx, mkIP("b1", "agent-b")); err != nil {
		t.Fatal(err)
	}
	// wiki page only in agent-c (distinct union across both tables).
	if err := s.UpsertPage(ctx, mkPage("p1", "agent-c")); err != nil {
		t.Fatal(err)
	}

	ids, err = s.ListAgentIDs(ctx)
	if err != nil {
		t.Fatalf("ListAgentIDs: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, want := range []string{"agent-a", "agent-b", "agent-c"} {
		if !got[want] {
			t.Errorf("missing agent %q in %v", want, ids)
		}
	}
	if len(ids) != 3 {
		t.Errorf("agent ids = %v, want exactly 3 distinct", ids)
	}
}
