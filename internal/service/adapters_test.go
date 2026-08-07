package service

import (
	"context"
	"testing"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/websearch"
)

// stubWebTool is a minimal websearch.Tool for registry tests.
type stubWebTool struct{ name string }

func (s *stubWebTool) Name() string { return s.name }

func (s *stubWebTool) Search(_ context.Context, _ string, _ int) ([]websearch.SearchItem, error) {
	return nil, nil
}

func TestNewWebSearchRegistryDefaultsToMyAgent(t *testing.T) {
	reg := newWebSearchRegistry(config.VerifyConfig{UseWebSearch: true, SearchMax: 5})
	if reg.Active() != "myagent" {
		t.Errorf("active = %q, want myagent", reg.Active())
	}
	if _, err := reg.Search(context.Background(), "q", 3); err != nil {
		t.Fatalf("search through default tool: %v", err)
	}
}

func TestNewWebSearchRegistryExtraToolSelectable(t *testing.T) {
	reg := newWebSearchRegistry(
		config.VerifyConfig{UseWebSearch: true, SearchMax: 5, WebTool: "custom"},
		&stubWebTool{name: "custom"},
	)
	if reg.Active() != "custom" {
		t.Errorf("active = %q, want custom", reg.Active())
	}
}

func TestNewWebSearchRegistryDisabled(t *testing.T) {
	// Web search disabled → registry with no active tool; Search returns nil
	// items (verify degrades gracefully).
	reg := newWebSearchRegistry(config.VerifyConfig{UseWebSearch: false})
	if reg.Active() != "" {
		t.Errorf("active = %q, want empty when disabled", reg.Active())
	}
	if items, err := reg.Search(context.Background(), "q", 3); err != nil || items != nil {
		t.Errorf("disabled search = %v, %v; want nil,nil", items, err)
	}
}
