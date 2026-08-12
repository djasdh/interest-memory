package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/djasdh/interest-memory/internal/websearch"

	"github.com/djasdh/my-agent-core/mcpclient"
	"github.com/djasdh/my-agent-core/types"
)

// mcpSearchTool adapts an MCP server's search tool into a websearch.Tool.
// The MCP tool is expected to accept a "query" (string) and optional
// "max_results" (number) and return JSON:
//
//	{"results":[{"title","url","snippet"}, ...]}
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
	return parseMCPResults(out)
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
