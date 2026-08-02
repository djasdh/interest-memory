package vec

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func newVec(t *testing.T) (*SQLiteVec, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	v, err := NewSQLiteVec(db, 3)
	if err != nil {
		t.Fatalf("NewSQLiteVec: %v", err)
	}
	if !v.Available() {
		t.Skip("sqlite-vec not available in this environment")
	}
	return v, db
}

func TestUpsertAndSearch(t *testing.T) {
	v, _ := newVec(t)
	ctx := context.Background()

	// Three entries; query closest to [1,0,0].
	entries := []Entry{
		{ID: "p1", AgentID: "ag", Kind: "wiki_page", Vector: []float32{1, 0, 0}, Metadata: map[string]string{"title": "A"}},
		{ID: "p2", AgentID: "ag", Kind: "wiki_page", Vector: []float32{0, 1, 0}, Metadata: map[string]string{"title": "B"}},
		{ID: "p3", AgentID: "ag", Kind: "wiki_page", Vector: []float32{0, 0, 1}, Metadata: map[string]string{"title": "C"}},
		{ID: "i1", AgentID: "ag", Kind: "interest_point", Vector: []float32{0.9, 0.1, 0}, Metadata: map[string]string{}},
	}
	for _, e := range entries {
		if err := v.Upsert(ctx, e); err != nil {
			t.Fatalf("Upsert %s: %v", e.ID, err)
		}
	}

	hits, err := v.Search(ctx, "ag", []float32{1, 0, 0}, 4)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].ID != "p1" {
		t.Errorf("top hit = %s, want p1 (got %+v)", hits[0].ID, hits)
	}
	// p1 should score higher than p3.
	if hits[0].Score <= hits[len(hits)-1].Score {
		t.Errorf("score ordering wrong: %+v", hits)
	}
	// Both kinds should appear.
	var kinds = map[string]bool{}
	for _, h := range hits {
		kinds[h.Kind] = true
	}
	if !kinds["wiki_page"] || !kinds["interest_point"] {
		t.Errorf("kinds = %v, want both wiki_page and interest_point", kinds)
	}

	// Agent isolation: querying agent-b yields nothing.
	hitsB, _ := v.Search(ctx, "agent-b", []float32{1, 0, 0}, 4)
	if len(hitsB) != 0 {
		t.Errorf("cross-agent leak: %+v", hitsB)
	}
}

// TestUpsertIdempotent guards the DELETE+INSERT fix: re-upserting the same id
// must succeed and replace the vector (vec0's INSERT OR REPLACE previously
// failed with UNIQUE constraint, breaking pipeline retries).
func TestUpsertIdempotent(t *testing.T) {
	v, _ := newVec(t)
	ctx := context.Background()
	if err := v.Upsert(ctx, Entry{ID: "p1", AgentID: "ag", Kind: "wiki_page", Vector: []float32{1, 0, 0}}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	// Same id, different vector — must replace, not fail.
	if err := v.Upsert(ctx, Entry{ID: "p1", AgentID: "ag", Kind: "wiki_page", Vector: []float32{0, 1, 0}}); err != nil {
		t.Fatalf("second Upsert (same id): %v", err)
	}
	hits, err := v.Search(ctx, "ag", []float32{0, 1, 0}, 4)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "p1" {
		t.Fatalf("after re-upsert hits = %+v, want p1 on top", hits)
	}
	// Exactly one row for p1.
	rows, err := v.db.QueryContext(ctx, "SELECT COUNT(*) FROM vec_wiki_page WHERE id = 'p1'")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("vec_wiki_page rows for p1 = %d, want 1", n)
		}
	}
}

func TestDelete(t *testing.T) {
	v, _ := newVec(t)
	ctx := context.Background()
	if err := v.Upsert(ctx, Entry{ID: "p1", AgentID: "ag", Kind: "wiki_page", Vector: []float32{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := v.Delete(ctx, "ag", "p1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, _ := v.Search(ctx, "ag", []float32{1, 0, 0}, 4)
	if len(hits) != 0 {
		t.Errorf("after delete hits = %+v, want empty", hits)
	}
}

func TestFallbackKeywords(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	f, err := NewFallback(db)
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}
	ctx := context.Background()
	f.Upsert(ctx, Entry{ID: "p1", AgentID: "ag", Kind: "wiki_page",
		Metadata: map[string]string{"title": "CAP 定理", "body": "分布式系统一致性"}})
	hits, err := f.SearchByKeywords(ctx, "ag", "分布式", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("keyword hits = %+v err=%v", hits, err)
	}
	if hits[0].ID != "p1" {
		t.Errorf("hit id = %s, want p1", hits[0].ID)
	}
}
