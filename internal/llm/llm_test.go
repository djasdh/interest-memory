package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"interest-memory/internal/config"
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
