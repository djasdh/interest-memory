package store

import (
	"context"
	"testing"
)

func TestUsageAccumulateByDay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddUsage(ctx, "2026-08-13", 100, 40, 25); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	if err := s.AddUsage(ctx, "2026-08-13", 50, 10, 5); err != nil {
		t.Fatalf("AddUsage 2: %v", err)
	}
	if err := s.AddUsage(ctx, "2026-08-14", 1, 1, 0); err != nil {
		t.Fatalf("AddUsage 3: %v", err)
	}

	got, err := s.GetUsage(ctx, "2026-08-13")
	if err != nil || got == nil {
		t.Fatalf("GetUsage: %v (got=%v)", err, got)
	}
	if got.Input != 150 || got.Output != 50 || got.CacheHit != 30 {
		t.Errorf("day usage = %+v, want input=150 output=50 cacheHit=30", got)
	}

	all, err := s.ListUsage(ctx, "")
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	if len(all) != 2 || all[0].Date != "2026-08-13" || all[1].Date != "2026-08-14" {
		t.Errorf("ListUsage = %+v, want 2 rows ordered by date", all)
	}

	since, err := s.ListUsage(ctx, "2026-08-14")
	if err != nil {
		t.Fatalf("ListUsage since: %v", err)
	}
	if len(since) != 1 || since[0].Date != "2026-08-14" {
		t.Errorf("ListUsage since = %+v, want 1 row (08-14)", since)
	}

	missing, err := s.GetUsage(ctx, "1999-01-01")
	if err != nil || missing != nil {
		t.Errorf("GetUsage absent = %v, %v; want nil, nil", missing, err)
	}
}
