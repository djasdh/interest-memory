package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/usage"
)

// Message is one chat turn in OpenAI format.
type Message struct {
	Role    string `json:"role"` // user | assistant | system
	Content string `json:"content"`
}

// ChatRequest is a non-streaming chat completion request.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// ChatUsage is the token accounting block of an OpenAI-compatible response.
// PromptCacheHitTokens is the DeepSeek prompt_cache_hit_tokens field (input
// tokens served from the provider's prompt cache — the direct cost saving).
type ChatUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
}

// ChatResponse mirrors the OpenAI chat completion response (non-streaming).
type ChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage *ChatUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Client is a fully-configurable OpenAI-compatible chat client.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	maxTokens  int
	httpClient *http.Client
	tracker    *usage.Tracker
}

// New creates a chat client from config. apiKey is resolved from the config's
// declared env var at construction time.
func New(cfg config.LLMConfig) *Client {
	return &Client{
		baseURL:    cfg.BaseURL,
		apiKey:     config.APIKey(cfg.APIKeyEnv),
		model:      cfg.Model,
		maxTokens:  cfg.MaxTokens,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// SetTracker wires an optional usage tracker; token deltas are reported after
// every completion.
func (c *Client) SetTracker(t *usage.Tracker) { c.tracker = t }

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

// Retry policy for transient HTTP failures (status >= 500, network errors).
// Applies to Chat (and thus ChatJSON); 4xx and decode errors are not retried.
const (
	maxRetries     = 3 // additional attempts after the first
	retryBaseDelay = 200 * time.Millisecond
)

// Chat performs a non-streaming completion and returns the assistant text.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (string, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	if req.MaxTokens == nil && c.maxTokens > 0 {
		mt := c.maxTokens
		req.MaxTokens = &mt
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	var lastErr error
	attempts := 1 + maxRetries
	for i := 0; i < attempts; i++ {
		text, retry, err := c.doChat(ctx, body)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !retry {
			return "", err
		}
		// Exponential backoff between attempts: base × 2^(attempt-1).
		delay := retryBaseDelay << uint(i)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

// doChat performs one HTTP round-trip. retry=true means the failure is
// transient (5xx / network) and worth retrying; 4xx and decode/validation
// failures are final.
func (c *Client) doChat(ctx context.Context, body []byte) (string, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Network failure — transient, retry.
		return "", true, fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", true, fmt.Errorf("llm: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("llm: status %d: %s", resp.StatusCode, truncate(string(data), 300))
		if resp.StatusCode >= 500 {
			return "", true, fmt.Errorf("%s", msg)
		}
		return "", false, fmt.Errorf("%s", msg)
	}

	var cr ChatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", false, fmt.Errorf("llm: decode: %w", err)
	}
	if cr.Error != nil {
		return "", false, fmt.Errorf("llm: api error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", false, fmt.Errorf("llm: empty choices")
	}
	if c.tracker != nil && cr.Usage != nil {
		c.tracker.Add(usage.Usage{
			Input:    int64(cr.Usage.PromptTokens),
			Output:   int64(cr.Usage.CompletionTokens),
			CacheHit: int64(cr.Usage.PromptCacheHitTokens),
		})
	}
	return cr.Choices[0].Message.Content, false, nil
}

// ChatJSON asks the model to produce a JSON payload and unmarshals it into out.
func (c *Client) ChatJSON(ctx context.Context, messages []Message, out any) error {
	text, err := c.Chat(ctx, ChatRequest{Messages: messages})
	if err != nil {
		return err
	}
	// Tolerate a ```json fence around the payload.
	text = stripFence(text)
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("llm: json decode %q: %w", truncate(text, 200), err)
	}
	return nil
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Byte-level cuts can split a multi-byte UTF-8 rune; back off to the
	// previous rune boundary so error messages stay valid UTF-8.
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		_, size := utf8.DecodeLastRuneInString(cut)
		cut = cut[:len(cut)-size]
	}
	return cut + "..."
}
