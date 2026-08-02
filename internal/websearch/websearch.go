// Package websearch provides a registerable set of web tools (search
// backends) and the narrow Searcher surface the correction layer consumes.
package websearch

import (
	"context"
	"fmt"
	"sync"
)

// SearchItem is one web hit used as evidence.
type SearchItem struct {
	Title   string
	URL     string
	Snippet string
	Source  string
}

// Searcher is the narrow search surface verify#1 depends on. Keep it
// implementation-agnostic so any registered tool can back it.
type Searcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchItem, error)
}

// Tool is a named, registerable search backend.
type Tool interface {
	Name() string
	Searcher
}

// Registry holds named web tools and routes Search to the active one. The
// first registered tool becomes the default active tool; SetActive switches.
// Safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	tools  map[string]Tool
	active string
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool by name. Duplicate names are rejected.
func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if name == "" {
		return fmt.Errorf("websearch: tool name cannot be empty")
	}
	if _, ok := r.tools[name]; ok {
		return fmt.Errorf("websearch: tool %q already registered", name)
	}
	r.tools[name] = t
	if r.active == "" {
		r.active = name
	}
	return nil
}

// SetActive switches the active tool. Unknown names are rejected.
func (r *Registry) SetActive(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return fmt.Errorf("websearch: unknown tool %q", name)
	}
	r.active = name
	return nil
}

// Active returns the name of the active tool ("" when none registered).
func (r *Registry) Active() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// Search routes to the active tool. Returns nil items when no tool is
// active (callers treat this as "no evidence", not an error).
func (r *Registry) Search(ctx context.Context, query string, maxResults int) ([]SearchItem, error) {
	r.mu.RLock()
	t := r.tools[r.active]
	r.mu.RUnlock()
	if t == nil {
		return nil, nil
	}
	return t.Search(ctx, query, maxResults)
}
