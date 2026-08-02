package fork

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"interest-memory/internal/config"
	"interest-memory/internal/llm"
)

// mockLLM implements LLM by capturing the prompt and returning canned
// candidates (or an error).
type mockLLM struct {
	prompt   string
	cands    []Candidate
	chatErr  error
	callN    int
}

func (m *mockLLM) ChatJSON(_ context.Context, messages []llm.Message, out any) error {
	m.callN++
	if len(messages) > 0 {
		m.prompt = messages[0].Content
	}
	if m.chatErr != nil {
		return m.chatErr
	}
	*(out.(*[]Candidate)) = m.cands
	return nil
}

func turnsFrom(contents ...string) []llm.Message {
	out := make([]llm.Message, 0, len(contents))
	for _, c := range contents {
		out = append(out, llm.Message{Role: "user", Content: c})
	}
	return out
}

func TestSplitWindows(t *testing.T) {
	msgs := turnsFrom("a", "b", "c", "d", "e", "f", "g", "h")

	tests := []struct {
		name string
		in   []llm.Message
		n    int
		want int // number of windows
	}{
		{name: "nil input", in: nil, n: 3, want: 0},
		{name: "empty", in: []llm.Message{}, n: 3, want: 0},
		{name: "exact divide", in: msgs, n: 4, want: 2},
		{name: "remainder", in: msgs, n: 3, want: 3},
		{name: "single window", in: msgs, n: 100, want: 1},
		{name: "zero fallback", in: msgs, n: 0, want: 1},
		{name: "negative fallback", in: msgs, n: -5, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitWindows(tt.in, tt.n)
			if len(got) != tt.want {
				t.Fatalf("SplitWindows windows = %d, want %d", len(got), tt.want)
			}
			// Concatenation must preserve original order/content.
			var joined []llm.Message
			for _, w := range got {
				joined = append(joined, w...)
			}
			if tt.want == 0 {
				if len(joined) != 0 {
					t.Errorf("windows concatenation = %d entries, want 0", len(joined))
				}
				return
			}
			if !reflect.DeepEqual(joined, tt.in) {
				t.Errorf("windows concatenation != input")
			}
		})
	}
}

func TestSplitWindowsZeroFallbackSize(t *testing.T) {
	msgs := turnsFrom("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k")
	got := SplitWindows(msgs, 0) // fallback to 10
	if len(got) != 2 {
		t.Fatalf("fallback windows = %d, want 2 (10-turn windows)", len(got))
	}
	if len(got[0]) != 10 || len(got[1]) != 1 {
		t.Errorf("fallback window sizes = [%d, %d], want [10, 1]", len(got[0]), len(got[1]))
	}
}

func TestAnalyzeFiltersLowConfidence(t *testing.T) {
	m := &mockLLM{cands: []Candidate{
		{Topic: "keep", Confidence: 0.9},
		{Topic: "drop-low", Confidence: 0.2},
		{Topic: "keep-edge", Confidence: 0.3}, // boundary ≥ min(0.3)
	}}
	a := NewAnalyzer(m, config.ForkConfig{WindowTurns: 10, MinConfidence: 0.3})
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{turnsFrom("x")})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 (low-confidence filtered)", len(got))
	}
	if got[0].Topic != "keep" || got[1].Topic != "keep-edge" {
		t.Errorf("got topics %q, want keep/keep-edge", []string{got[0].Topic, got[1].Topic})
	}
	if m.callN != 1 {
		t.Errorf("LLM calls = %d, want 1", m.callN)
	}
	if m.prompt == "" {
		t.Error("prompt not captured")
	}
}

func TestAnalyzeMergesAcrossWindows(t *testing.T) {
	m := &mockLLM{cands: []Candidate{{Topic: "w1", Confidence: 0.9}}}
	a := NewAnalyzer(m, config.ForkConfig{WindowTurns: 10})
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{
		turnsFrom("one"),
		turnsFrom("two"),
		turnsFrom("three"),
	})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("candidates = %d, want 3 (one per window)", len(got))
	}
	if m.callN != 3 {
		t.Errorf("LLM calls = %d, want 3", m.callN)
	}
}

func TestAnalyzeMaxCandidatesPerWindow(t *testing.T) {
	// max_candidates_per_window=1 → each window keeps at most its top candidate.
	m := &mockLLM{cands: []Candidate{
		{Topic: "a", Confidence: 0.9},
		{Topic: "b", Confidence: 0.8},
	}}
	a := NewAnalyzer(m, config.ForkConfig{WindowTurns: 10, MaxCandidates: 1})
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{
		turnsFrom("one"),
		turnsFrom("two"),
	})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 (one per window, capped at 1 each)", len(got))
	}
	for _, c := range got {
		if c.Topic != "a" {
			t.Errorf("candidate topic = %s, want top of window", c.Topic)
		}
	}
	if m.callN != 2 {
		t.Errorf("LLM calls = %d, want 2 (no early stop across windows)", m.callN)
	}
}

func TestAnalyzeSkipsEmptyWindow(t *testing.T) {
	m := &mockLLM{cands: []Candidate{{Topic: "x", Confidence: 0.9}}}
	a := NewAnalyzer(m, config.ForkConfig{WindowTurns: 10})
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{
		turnsFrom("real"),
		turnsFrom(), // no content → no LLM call
	})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1", len(got))
	}
	if m.callN != 1 {
		t.Errorf("LLM calls = %d, want 1 (empty window skipped)", m.callN)
	}
}

func TestAnalyzeLLMErrorPropagates(t *testing.T) {
	m := &mockLLM{chatErr: errors.New("boom")}
	a := NewAnalyzer(m, config.ForkConfig{WindowTurns: 10})
	_, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{turnsFrom("x")})
	if err == nil {
		t.Fatal("expected error from LLM, got nil")
	}
}

func TestSummarize(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "system", Content: "ignored"},
		{Role: "user", Content: ""}, // empty skipped
	}
	got := summarize(msgs)
	if got == "" {
		t.Fatal("summarize returned empty")
	}
	for _, want := range []string{"[USER]: hello", "[ASSISTANT]: hi there"} {
		if !contains(got, want) {
			t.Errorf("summarize missing %q\n---\n%s", want, got)
		}
	}
	if contains(got, "[SYSTEM]") || contains(got, "[USER]: ignored") {
		t.Errorf("summarize should skip system/empty messages\n---\n%s", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
