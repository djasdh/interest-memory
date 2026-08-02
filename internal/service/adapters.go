package service

import (
	"context"

	"interest-memory/internal/config"
	"interest-memory/internal/verify"

	_ "my-agent-core/web_search/provider" // register the 12 search providers

	"my-agent-core/provider"
	"my-agent-core/types"
	websearch "my-agent-core/web_search/core"
)

// webSearchSearcher adapts my-agent-core web_search into verify.Searcher.
// It is nil-returning on any failure so verify degrades gracefully.
type webSearchSearcher struct {
	enabled   bool
	maxResults int
}

func newWebSearchSearcher(cfg config.VerifyConfig) verify.Searcher {
	if !cfg.UseWebSearch {
		return nil
	}
	max := cfg.SearchMax
	if max <= 0 {
		max = 5
	}
	return &webSearchSearcher{enabled: true, maxResults: max}
}

func (s *webSearchSearcher) Search(ctx context.Context, query string, maxResults int) ([]verify.SearchItem, error) {
	if !s.enabled {
		return nil, nil
	}
	if maxResults <= 0 {
		maxResults = s.maxResults
	}
	res, err := websearch.SearchWithContext(ctx, query, maxResults)
	if err != nil || res == nil {
		return nil, err
	}
	out := make([]verify.SearchItem, 0, len(res.Results))
	for _, it := range res.Results {
		out = append(out, verify.SearchItem{
			Title:   it.Title,
			URL:     it.URL,
			Snippet: it.Snippet,
			Source:  it.Source,
		})
	}
	return out, nil
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
