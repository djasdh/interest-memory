package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/djasdh/interest-memory/internal/websearch"

	"github.com/djasdh/my-agent-core/mcpclient"
	"github.com/djasdh/my-agent-core/types"
)

// mcpSearchTool adapts an MCP server's search tool into a websearch.Tool.
// The MCP tool is expected to accept a "query" (string) and optional
// "max_results" (number) and return either:
//
//  1. JSON: {"results":[{"title","url","snippet"}, ...]}   (structured mode)
//  2. Markdown text: "# Search: ...\n## 结果\n1. [Title](url) · source · snippet"
//     (human/agent-friendly mode, e.g. the agent-search gateway)
//
// Any failure returns an error so verify degrades to LLM-only evidence.
type mcpSearchTool struct {
	mgr        *mcpclient.Manager
	searchTool string
}

// Name implements websearch.Tool.
func (t *mcpSearchTool) Name() string { return "mcp" }

// Search implements websearch.Tool.
func (t *mcpSearchTool) Search(ctx context.Context, query string, maxResults int) ([]websearch.SearchItem, error) {
	tool := t.findTool()
	if tool == nil {
		return nil, fmt.Errorf("mcp: search tool %q not found", t.searchTool)
	}
	args := types.ArgsMap{"query": query}
	if maxResults > 0 {
		args["max_results"] = float64(maxResults)
	}
	out, err := tool.Execute(types.Context{}, args, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: search %q: %w", t.searchTool, err)
	}

	// Try structured JSON first (the documented schema), then fall back to
	// parsing markdown text (agent-search gateway style output).
	if items, err := parseMCPResults(out); err == nil {
		return items, nil
	}
	items, perr := parseMarkdownSearchResults(out, maxResults)
	if perr != nil {
		// Neither JSON nor markdown: degrade with the original parse error.
		return nil, fmt.Errorf("mcp: search output not parseable: %v (json: %v)", perr, err)
	}
	return items, nil
}

// findTool locates the target search tool by name; when searchTool is empty it
// falls back to the first tool whose name contains "search".
func (t *mcpSearchTool) findTool() *types.Tool {
	for _, tool := range t.mgr.Tools() {
		if t.searchTool != "" {
			if tool.Name == t.searchTool {
				return &tool
			}
			continue
		}
		if strings.Contains(strings.ToLower(tool.Name), "search") {
			return &tool
		}
	}
	return nil
}

type mcpResults struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	} `json:"results"`
}

// parseMCPResults decodes the agreed search JSON schema into SearchItems.
func parseMCPResults(out string) ([]websearch.SearchItem, error) {
	var r mcpResults
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, fmt.Errorf("mcp: search returned non-JSON output: %w", err)
	}
	items := make([]websearch.SearchItem, 0, len(r.Results))
	for _, it := range r.Results {
		items = append(items, websearch.SearchItem{Title: it.Title, URL: it.URL, Snippet: it.Snippet})
	}
	return items, nil
}

// markdownLinkRE matches "[Title](url)" links inside markdown search results.
var markdownLinkRE = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

// parseMarkdownSearchResults parses agent-search-gateway style output:
//
//	# Search: <query>
//	*N 个结果 · <ms>*
//
//	## 结果
//
//	1. [Title](url) · source · score 1.00
//	   Snippet line...
//
// It extracts each "[Title](url)" link plus the following snippet line (the
// text on the same line after the link, and optionally the next non-empty
// line). Results are capped at maxResults (0 = no cap).
func parseMarkdownSearchResults(out string, maxResults int) ([]websearch.SearchItem, error) {
	lines := strings.Split(out, "\n")
	var items []websearch.SearchItem
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		loc := markdownLinkRE.FindStringSubmatchIndex(line)
		if loc == nil {
			continue
		}
		title := line[loc[2]:loc[3]]
		url := line[loc[4]:loc[5]]

		// Snippet = text after the link on the same line, trimmed of
		// "· source · score" noise; then append the next non-empty line if any.
		after := strings.TrimSpace(line[loc[1]:])
		after = strings.TrimPrefix(after, "·")
		after = strings.TrimSpace(after)
		snippet := after
		if j := i + 1; j < len(lines) {
			next := strings.TrimSpace(lines[j])
			if next != "" && !strings.HasPrefix(next, "#") && !strings.HasPrefix(next, "---") {
				if snippet != "" {
					snippet += " "
				}
				snippet += next
			}
		}

		items = append(items, websearch.SearchItem{
			Title:   title,
			URL:     url,
			Snippet: truncate(snippet, 200),
		})
		if maxResults > 0 && len(items) >= maxResults {
			break
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("mcp: no markdown links found in search output")
	}
	return items, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
