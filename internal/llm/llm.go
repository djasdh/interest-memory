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

	"github.com/djasdh/interest-memory/internal/config"
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

// ChatResponse mirrors the OpenAI chat completion response (non-streaming).
type ChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
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

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(string(data), 300))
	}

	var cr ChatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("llm: decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("llm: api error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
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
	return s[:n] + "..."
}
