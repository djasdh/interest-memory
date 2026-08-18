package service

import (
	"strings"
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

func TestParseMarkdownSearchResults(t *testing.T) {
	out := `# Search: OKF v0.2
*3 个结果 · 917ms*

## 结果

1. [OKF v0.2 adds trust signals | Google Cloud Blog](https://cloud.google.com/blog/products/data-analytics/okf-v0-2-adds-trust-signals) · google cse · score 1.00
   With the Open Knowledge Format v0.2 spec, we added fields that signal trust.
2. [Open Knowledge Format (OKF): The Complete 2026 Guide](https://witscode.com/open-knowledge-format) · duckduckgo · score 0.83
   An open, human- and agent-friendly format for representing knowledge.
3. [Google OKF GitHub](https://github.com/GoogleCloudPlatform/knowledge-catalog) · duckduckgo · score 0.80`

	items, err := parseMarkdownSearchResults(out, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(items), items)
	}
	if items[0].Title != "OKF v0.2 adds trust signals | Google Cloud Blog" {
		t.Errorf("title[0] = %q", items[0].Title)
	}
	if items[0].URL != "https://cloud.google.com/blog/products/data-analytics/okf-v0-2-adds-trust-signals" {
		t.Errorf("url[0] = %q", items[0].URL)
	}
	if items[0].Snippet == "" || !strings.Contains(items[0].Snippet, "trust") {
		t.Errorf("snippet[0] = %q, want trust signal snippet", items[0].Snippet)
	}
	if items[1].URL != "https://witscode.com/open-knowledge-format" {
		t.Errorf("url[1] = %q", items[1].URL)
	}
}

func TestParseMarkdownSearchResultsMaxResults(t *testing.T) {
	out := `# Search: test
1. [A](https://a.example) · bing
2. [B](https://b.example) · bing
3. [C](https://c.example) · bing`

	items, err := parseMarkdownSearchResults(out, 2)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
}

func TestParseMarkdownSearchResultsEmpty(t *testing.T) {
	if _, err := parseMarkdownSearchResults("no links here\njust text\n", 0); err == nil {
		t.Errorf("want error on no links")
	}
}

func TestSearchTriesJSONThenMarkdown(t *testing.T) {
	// JSON-structured output should parse via JSON path.
	jsonOut := `{"results":[{"title":"A","url":"https://a","snippet":"s1"}]}`
	if _, err := parseMCPResults(jsonOut); err != nil {
		t.Errorf("json parse: %v", err)
	}
	// Markdown output should parse via markdown path.
	mdOut := `# Search: x
1. [A](https://a.example) · bing
   snippet here`
	if _, err := parseMarkdownSearchResults(mdOut, 0); err != nil {
		t.Errorf("md parse: %v", err)
	}
}
