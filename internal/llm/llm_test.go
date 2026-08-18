package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djasdh/interest-memory/internal/config"
)

func testLLMConfig(srvURL string) config.LLMConfig {
	return config.LLMConfig{BaseURL: srvURL, APIKeyEnv: "IM_LLM_TEST_KEY", Model: "test-model", MaxTokens: 100}
}

func TestChat(t *testing.T) {
	var gotModel string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello back"}}]}`))
	}))
	defer srv.Close()

	t.Setenv("IM_LLM_TEST_KEY", "secret-key")
	c := New(testLLMConfig(srv.URL))
	text, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if text != "hello back" {
		t.Errorf("text = %q", text)
	}
	if gotModel != "test-model" {
		t.Errorf("model = %q, want test-model", gotModel)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestChatJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n{\\\"topics\\\":[\\\"a\\\",\\\"b\\\"]}\\n```\"}}]}"))
	}))
	defer srv.Close()

	c := New(testLLMConfig(srv.URL))
	var out struct {
		Topics []string `json:"topics"`
	}
	if err := c.ChatJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, &out); err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if len(out.Topics) != 2 || out.Topics[0] != "a" {
		t.Errorf("out = %+v", out)
	}
}

func TestChatErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	c := New(testLLMConfig(srv.URL))
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("err = %v, want bad key error", err)
	}
}

func TestChatRetriesTransientErrors(t *testing.T) {
	// First call returns 500, then succeeds. Chat must retry (exponential
	// backoff) and return the eventual success.
	reqs := 0
	var mu sync.Mutex
	times := make([]time.Time, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		times = append(times, time.Now())
		n := reqs
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := New(testLLMConfig(srv.URL))
	text, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if text != "ok" {
		t.Errorf("text = %q, want ok", text)
	}
	mu.Lock()
	n := reqs
	mu.Unlock()
	if n != 2 {
		t.Errorf("requests = %d, want 2 (1 failure + 1 retry)", n)
	}
	// Exponential backoff: the retry must not happen immediately (>= base).
	mu.Lock()
	d := times[1].Sub(times[0])
	mu.Unlock()
	if d < retryBaseDelay {
		t.Errorf("retry delay = %v, want >= %v (exponential backoff)", d, retryBaseDelay)
	}
}

func TestChatGivesUpAfterMaxRetries(t *testing.T) {
	// Server always 500 → retries exhaust, last error surfaces.
	reqs := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()
	c := New(testLLMConfig(srv.URL))
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	mu.Lock()
	n := reqs
	mu.Unlock()
	// 1 initial + maxRetries retries.
	if n != 1+maxRetries {
		t.Errorf("requests = %d, want %d", n, 1+maxRetries)
	}
}

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "emb-model" {
			t.Errorf("embed model = %q", req.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		// Echo two vectors sized to the input count.
		out := `{"data":[{"embedding":[0.1,0.2,0.3]},{"embedding":[0.4,0.5,0.6]}]}`
		w.Write([]byte(out))
	}))
	defer srv.Close()

	e := NewEmbedder(config.EmbeddingConfig{
		BaseURL: srv.URL, APIKeyEnv: "IM_EMB_TEST_KEY", Model: "emb-model", Dimensions: 3,
	})
	t.Setenv("IM_EMB_TEST_KEY", "k")
	vecs, err := e.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 || vecs[1][0] != 0.4 {
		t.Errorf("vecs = %+v", vecs)
	}
	dim := e.Dimensions()
	if dim != 3 {
		t.Errorf("Dimensions = %d, want 3", dim)
	}
}

func TestEmbedCacheHitsSingle(t *testing.T) {
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()
	e := NewEmbedder(config.EmbeddingConfig{BaseURL: srv.URL, APIKeyEnv: "IM_EMB_TEST_KEY", Model: "m", Dimensions: 3})
	t.Setenv("IM_EMB_TEST_KEY", "k")

	if _, err := e.Embed(context.Background(), "same text"); err != nil {
		t.Fatalf("Embed 1: %v", err)
	}
	if _, err := e.Embed(context.Background(), "same text"); err != nil {
		t.Fatalf("Embed 2: %v", err)
	}
	if reqs != 1 {
		t.Errorf("embed requests = %d, want 1 (second call cached)", reqs)
	}
}

func TestEmbedBatchPartialHits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		out := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			out[i] = map[string]any{"embedding": []float32{0.5, 0.5, 0.5}}
		}
		data, _ := json.Marshal(map[string]any{"data": out})
		w.Write(data)
	}))
	defer srv.Close()
	e := NewEmbedder(config.EmbeddingConfig{BaseURL: srv.URL, APIKeyEnv: "IM_EMB_TEST_KEY", Model: "m", Dimensions: 3})
	t.Setenv("IM_EMB_TEST_KEY", "k")

	if _, err := e.Embed(context.Background(), "dup"); err != nil {
		t.Fatalf("Embed dup: %v", err)
	}
	vecs, err := e.EmbedBatch(context.Background(), []string{"dup", "new"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("len(vecs) = %d, want 2 (order preserved)", len(vecs))
	}
}

func TestEmbedCacheInvalidatedByModel(t *testing.T) {
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()
	e := NewEmbedder(config.EmbeddingConfig{BaseURL: srv.URL, APIKeyEnv: "IM_EMB_TEST_KEY", Model: "m1", Dimensions: 3})
	t.Setenv("IM_EMB_TEST_KEY", "k")
	if _, err := e.Embed(context.Background(), "x"); err != nil {
		t.Fatalf("Embed m1: %v", err)
	}
	e.model = "m2" // simulate config change; cache key includes model → no hit
	if _, err := e.Embed(context.Background(), "x"); err != nil {
		t.Fatalf("Embed m2: %v", err)
	}
	if reqs != 2 {
		t.Errorf("embed requests = %d, want 2 (model change invalidates cache)", reqs)
	}
}

func TestEmbedBatchConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		out := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			out[i] = map[string]any{"embedding": []float32{0.5, 0.5, 0.5}}
		}
		data, _ := json.Marshal(map[string]any{"data": out})
		w.Write(data)
	}))
	defer srv.Close()
	e := NewEmbedder(config.EmbeddingConfig{BaseURL: srv.URL, APIKeyEnv: "IM_EMB_TEST_KEY", Model: "m", Dimensions: 3})
	t.Setenv("IM_EMB_TEST_KEY", "k")

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				text := fmt.Sprintf("%s-%d", base, i)
				if _, err := e.Embed(context.Background(), text); err != nil {
					t.Errorf("Embed: %v", err)
				}
			}
		}(string(rune('a' + g)))
	}
	wg.Wait()
}
