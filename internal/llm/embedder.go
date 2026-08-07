package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/djasdh/interest-memory/internal/config"
)

// Embedder produces vector embeddings for text via an OpenAI-compatible
// embeddings endpoint. Fully configurable (base_url/key/model/dimensions).
type Embedder struct {
	baseURL    string
	apiKey     string
	model      string
	dimensions int
	httpClient *http.Client
}

// NewEmbedder creates an embedding client from config.
func NewEmbedder(cfg config.EmbeddingConfig) *Embedder {
	return &Embedder{
		baseURL:    cfg.BaseURL,
		apiKey:     config.APIKey(cfg.APIKeyEnv),
		model:      cfg.Model,
		dimensions: cfg.Dimensions,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// Dimensions returns the configured vector dimensionality.
func (e *Embedder) Dimensions() int { return e.dimensions }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed computes a single embedding vector for text.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embedder: empty result")
	}
	return vecs[0], nil
}

// EmbedBatch computes embeddings for multiple texts in one request.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embedder: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("embedder: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedder: status %d: %s", resp.StatusCode, truncate(string(data), 300))
	}

	var er embedResponse
	if err := json.Unmarshal(data, &er); err != nil {
		return nil, fmt.Errorf("embedder: decode: %w", err)
	}
	if er.Error != nil {
		return nil, fmt.Errorf("embedder: api error: %s", er.Error.Message)
	}
	out := make([][]float32, 0, len(er.Data))
	for _, d := range er.Data {
		out = append(out, d.Embedding)
	}
	return out, nil
}
