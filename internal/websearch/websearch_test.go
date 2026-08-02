package websearch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// fakeTool records calls and returns canned items.
type fakeTool struct {
	name string
	items []SearchItem
	err   error
	calls atomic.Int32
}

func (f *fakeTool) Name() string { return f.name }

func (f *fakeTool) Search(_ context.Context, _ string, _ int) ([]SearchItem, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func TestRegisterAndRoute(t *testing.T) {
	reg := New()
	a := &fakeTool{name: "a", items: []SearchItem{{Title: "A"}}}
	b := &fakeTool{name: "b", items: []SearchItem{{Title: "B"}}}
	if err := reg.Register(a); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := reg.Register(b); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if err := reg.Register(a); err == nil {
		t.Fatal("expected duplicate register error")
	}

	if err := reg.SetActive("b"); err != nil {
		t.Fatalf("set active b: %v", err)
	}
	items, err := reg.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 || items[0].Title != "B" {
		t.Errorf("routed to wrong tool: %+v", items)
	}
	if b.calls.Load() != 1 || a.calls.Load() != 0 {
		t.Errorf("calls a=%d b=%d, want a=0 b=1", a.calls.Load(), b.calls.Load())
	}

	if err := reg.SetActive("nope"); err == nil {
		t.Fatal("expected unknown active error")
	}
	if err := reg.SetActive("a"); err != nil {
		t.Fatalf("set active a: %v", err)
	}
	if _, err := reg.Search(context.Background(), "q", 3); err != nil {
		t.Fatalf("search a: %v", err)
	}
}

func TestEmptyRegistrySearch(t *testing.T) {
	reg := New()
	items, err := reg.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("empty search error: %v", err)
	}
	if items != nil {
		t.Errorf("expected nil items, got %+v", items)
	}
}

func TestToolErrorPropagates(t *testing.T) {
	reg := New()
	want := errors.New("boom")
	if err := reg.Register(&fakeTool{name: "e", err: want}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetActive("e"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Search(context.Background(), "q", 3); !errors.Is(err, want) {
		t.Fatalf("search error = %v, want %v", err, want)
	}
}

func TestActiveDefaultFirstRegistered(t *testing.T) {
	reg := New()
	if reg.Active() != "" {
		t.Errorf("default active = %q, want empty", reg.Active())
	}
	_ = reg.Register(&fakeTool{name: "first"})
	if reg.Active() != "first" {
		t.Errorf("active after first register = %q, want first", reg.Active())
	}
	_ = reg.Register(&fakeTool{name: "second"})
	if reg.Active() != "first" {
		t.Errorf("active after second register = %q, want still first", reg.Active())
	}
}
