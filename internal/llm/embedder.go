package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/djasdh/interest-memory/internal/cache"
	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/usage"
)

// embedCacheCapacity bounds the in-process embedding LRU. Embeddings are
// deterministic per (model, dimensions, text); the cache dedupes repeated
// calls (recall, prelookup, wikiloop queries) across the process lifetime.
const embedCacheCapacity = 4096

// Embedder produces vector embeddings for text via an OpenAI-compatible
// embeddings endpoint. Fully configurable (base_url/key/model/dimensions).
type Embedder struct {
	baseURL    string
	apiKey     string
	model      string
	dimensions int
	httpClient *http.Client
	tracker    *usage.Tracker
	cache      *cache.Cache[string, []float32]
}

// NewEmbedder creates an embedding client from config.
func NewEmbedder(cfg config.EmbeddingConfig) *Embedder {
	e := &Embedder{
		baseURL:    cfg.BaseURL,
		apiKey:     config.APIKey(cfg.APIKeyEnv),
		model:      cfg.Model,
		dimensions: cfg.Dimensions,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
	e.cache = cache.New[string, []float32](embedCacheCapacity)
	return e
}

// SetTracker wires an optional usage tracker; embedding input tokens are
// reported after every request.
func (e *Embedder) SetTracker(t *usage.Tracker) { e.tracker = t }

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
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// embedKey returns the cache key for a text embedding: model + dimensions +
// content hash. A model or dimensions change automatically misses (prevents
// mismatched-dimension vectors from being reused across configs).
func embedKey(model string, dims int, text string) string {
	h := sha256.Sum256([]byte(text))
	return model + "\x00" + strconv.Itoa(dims) + "\x00" + hex.EncodeToString(h[:])
}

func (e *Embedder) cached(text string) ([]float32, bool) {
	if e.cache == nil {
		return nil, false
	}
	return e.cache.Get(embedKey(e.model, e.dimensions, text))
}

func (e *Embedder) storeCached(text string, v []float32) {
	if e.cache == nil {
		return
	}
	e.cache.Set(embedKey(e.model, e.dimensions, text), v)
}

// Embed computes a single embedding vector for text.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if v, ok := e.cached(text); ok {
		return v, nil
	}
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
	out := make([][]float32, len(texts))
	var missIdx []int
	var missTexts []string
	for i, t := range texts {
		if v, ok := e.cached(t); ok {
			out[i] = v
		} else {
			missIdx = append(missIdx, i)
			missTexts = append(missTexts, t)
		}
	}
	if len(missTexts) == 0 {
		return out, nil
	}
	body, err := json.Marshal(embedRequest{Model: e.model, Input: missTexts})
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
	for i, d := range er.Data {
		out[missIdx[i]] = d.Embedding
		e.storeCached(missTexts[i], d.Embedding)
	}
	if e.tracker != nil && er.Usage != nil {
		e.tracker.Add(usage.Usage{Input: int64(er.Usage.PromptTokens)})
	}
	return out, nil
}
