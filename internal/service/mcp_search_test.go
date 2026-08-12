package service

import (
	"testing"

	"github.com/djasdh/interest-memory/internal/websearch"
)

func TestParseMCPResults(t *testing.T) {
	items, err := parseMCPResults(`{"results":[{"title":"A","url":"https://a","snippet":"s1"},{"title":"B","url":"https://b","snippet":"s2"}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	if items[0].Title != "A" || items[0].URL != "https://a" || items[0].Snippet != "s1" {
		t.Errorf("items[0] = %+v", items[0])
	}
	if items[1].Title != "B" {
		t.Errorf("items[1] = %+v", items[1])
	}
}

func TestParseMCPResultsEmptyAndInvalid(t *testing.T) {
	items, err := parseMCPResults(`{"results":[]}`)
	if err != nil || len(items) != 0 {
		t.Errorf("empty: items=%v err=%v, want 0 items, nil err", items, err)
	}

	if _, err := parseMCPResults(`not json`); err == nil {
		t.Errorf("invalid: want error")
	}
}

var _ websearch.Tool = (*mcpSearchTool)(nil)
