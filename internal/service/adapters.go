package service

import (
	"context"

	"interest-memory/internal/config"
	"interest-memory/internal/websearch"

	_ "github.com/djasdh/my-agent-core/web_search/provider" // register the 12 search providers

	"github.com/djasdh/my-agent-core/provider"
	"github.com/djasdh/my-agent-core/types"
	websearchcore "github.com/djasdh/my-agent-core/web_search/core"
)

// myAgentWebTool adapts my-agent-core web_search into a registered websearch
// tool named "myagent". It is nil-returning on any failure so verify degrades
// gracefully.
type myAgentWebTool struct {
	maxResults int
}

func (t *myAgentWebTool) Name() string { return "myagent" }

func (t *myAgentWebTool) Search(ctx context.Context, query string, maxResults int) ([]websearch.SearchItem, error) {
	if maxResults <= 0 {
		maxResults = t.maxResults
	}
	res, err := websearchcore.SearchWithContext(ctx, query, maxResults)
	if err != nil || res == nil {
		return nil, err
	}
	out := make([]websearch.SearchItem, 0, len(res.Results))
	for _, it := range res.Results {
		out = append(out, websearch.SearchItem{
			Title:   it.Title,
			URL:     it.URL,
			Snippet: it.Snippet,
			Source:  it.Source,
		})
	}
	return out, nil
}

// newWebSearchRegistry builds the registerable network-tool registry: the
// default my-agent-core-backed "myagent" tool plus any caller-supplied extra
// tools, then activates the configured web_tool (or the first registered tool
// when the configured name is absent). When web search is disabled the
// registry stays empty (verify degrades to LLM-only evidence).
func newWebSearchRegistry(cfg config.VerifyConfig, extra ...websearch.Tool) *websearch.Registry {
	reg := websearch.New()
	if !cfg.UseWebSearch {
		return reg
	}
	_ = reg.Register(&myAgentWebTool{maxResults: cfg.SearchMax})
	for _, t := range extra {
		if t != nil {
			_ = reg.Register(t)
		}
	}
	// Activate the configured tool; fall back to the default active (first
	// registered) when the name is unknown or unset.
	if cfg.WebTool != "" && cfg.WebTool != "myagent" {
		_ = reg.SetActive(cfg.WebTool)
	}
	return reg
}

// buildWikiProvider constructs the my-agent-core provider used by the
// WikiWriter agent loop from interest-memory config. base_url already
// includes /v1 (stage 3 decision); keys come from the configured env vars.
func buildWikiProvider(cfg config.Config) *provider.Provider {
	llmKey := config.APIKey(cfg.LLM.APIKeyEnv)
	embKey := config.APIKey(cfg.Embedding.APIKeyEnv)

	model := types.Model{
		ID:        cfg.LLM.Model,
		BaseURL:   cfg.LLM.BaseURL,
		API:       provider.APIOpenAICompletions,
		MaxTokens: cfg.LLM.MaxTokens,
	}
	p := provider.NewConfiguredProvider(model, llmKey)
	p.WithEmbedding(cfg.Embedding.Model, cfg.Embedding.BaseURL, embKey)
	return p
}
