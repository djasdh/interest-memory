package fork

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"interest-memory/internal/config"
	"interest-memory/internal/llm"
)

// mockLLM implements LLM by capturing the prompt and returning canned
// candidates (or an error). Thread-safe: Analyze runs windows concurrently.
type mockLLM struct {
	mu      sync.Mutex
	prompt  string
	cands   []Candidate      // returned when perCall is empty
	perCall [][]Candidate    // consumed in order, one per ChatJSON call
	chatErr error
	callN   int
}

func (m *mockLLM) ChatJSON(_ context.Context, messages []llm.Message, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callN++
	if len(messages) > 0 {
		m.prompt = messages[0].Content
	}
	if m.chatErr != nil {
		return m.chatErr
	}
	var cs []Candidate
	if len(m.perCall) > 0 {
		cs = m.perCall[0]
		m.perCall = m.perCall[1:]
	} else {
		cs = m.cands
	}
	*(out.(*[]Candidate)) = cs
	return nil
}

func turnsFrom(contents ...string) []llm.Message {
	out := make([]llm.Message, 0, len(contents))
	for _, c := range contents {
		out = append(out, llm.Message{Role: "user", Content: c})
	}
	return out
}

// mixTurns interleaves assistant messages so tests exercise user-turn counting.
func mixTurns(userContents ...string) []llm.Message {
	var out []llm.Message
	for _, c := range userContents {
		out = append(out, llm.Message{Role: "user", Content: c})
		out = append(out, llm.Message{Role: "assistant", Content: "reply " + c})
	}
	return out
}

func analyzeCfg() config.ForkConfig {
	return config.ForkConfig{PrefixStep: 5, MaxWindows: 8, MaxConcurrency: 4}
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

func TestSplitPrefixWindowsByUserTurns(t *testing.T) {
	// 3 user turns < step 5 → single full window, no split.
	short := mixTurns("a", "b", "c")
	got := SplitPrefixWindows(short, 5, 8)
	if len(got) != 1 {
		t.Fatalf("short transcript windows = %d, want 1 (no split)", len(got))
	}
	if !reflect.DeepEqual(got[0], short) {
		t.Error("short transcript window should be the full input")
	}

	// 12 user turns, step 5 → [..pos5], [..pos10], full (assistant msgs don't count).
	msgs := mixTurns("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l")
	got = SplitPrefixWindows(msgs, 5, 8)
	if len(got) != 3 {
		t.Fatalf("windows = %d, want 3", len(got))
	}
	// pos5 = index 8 (a@0, reply, b@2, ... e@8); pos10 = index 18.
	if len(got[0]) != 9 || len(got[1]) != 19 || len(got[2]) != len(msgs) {
		t.Errorf("window sizes = [%d, %d, %d], want [9, 19, %d]",
			len(got[0]), len(got[1]), len(got[2]), len(msgs))
	}
	// Each window is a strict prefix of the next.
	for i := 1; i < len(got); i++ {
		if !reflect.DeepEqual(got[i-1], got[i][:len(got[i-1])]) {
			t.Errorf("window %d is not a prefix of window %d", i-1, i)
		}
	}
}

func TestSplitPrefixWindowsMaxWindows(t *testing.T) {
	msgs := mixTurns("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l")
	got := SplitPrefixWindows(msgs, 5, 2) // cap at 2 longest
	if len(got) != 2 {
		t.Fatalf("windows = %d, want 2 (capped)", len(got))
	}
	// Longest 2 windows: [..pos10] and full.
	if len(got[0]) != 19 || len(got[1]) != len(msgs) {
		t.Errorf("capped window sizes = [%d, %d], want [19, %d]", len(got[0]), len(got[1]), len(msgs))
	}
}

func TestSplitPrefixWindowsNoUserTurns(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "assistant", Content: "c"},
	}
	got := SplitPrefixWindows(msgs, 5, 8)
	if len(got) != 1 {
		t.Fatalf("windows = %d, want 1 (no user turns)", len(got))
	}
	if !reflect.DeepEqual(got[0], msgs) {
		t.Error("should be a single full window")
	}
}

func TestSplitPrefixWindowsEmpty(t *testing.T) {
	if got := SplitPrefixWindows(nil, 5, 8); got != nil {
		t.Errorf("nil input windows = %v, want nil", got)
	}
	if got := SplitPrefixWindows([]llm.Message{}, 5, 8); got != nil {
		t.Errorf("empty input windows = %v, want nil", got)
	}
}

func TestExtractParsesSubjective(t *testing.T) {
	// mockLLM returns Candidate with Subjective set — proves the JSON round-trip
	// and that Analyze passes it through.
	m := &mockLLM{cands: []Candidate{
		{Topic: "喜欢 Go", Confidence: 0.9, Subjective: true},
		{Topic: "Go 1.24 特性", Confidence: 0.8, Subjective: false},
	}}
	a := NewAnalyzer(m, analyzeCfg())
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{turnsFrom("x")})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
	if !got[0].Subjective || got[1].Subjective {
		t.Errorf("subjective flags = %v/%v, want true/false", got[0].Subjective, got[1].Subjective)
	}
	// Prompt should ask the model to judge subjectivity.
	if !contains(m.prompt, "subjective") {
		t.Error("prompt should mention subjective judgment")
	}
}

func TestAnalyzeMapsTurnRangeToGlobalIndex(t *testing.T) {
	// Global turns: a@0, r1@1, b@2, c@3(empty, skipped), r2@4, tool@5(skipped), d@6.
	// Rendered sequence (turn # → global index): 1→0(a), 2→1(r1), 3→2(b), 4→4(r2), 5→6(d).
	msgs := []llm.Message{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "r1"},
		{Role: "user", Content: "b"},
		{Role: "user", Content: "  "},
		{Role: "assistant", Content: "r2"},
		{Role: "tool", Content: "t"},
		{Role: "user", Content: "d"},
	}
	m := &mockLLM{cands: []Candidate{
		{Topic: "x", Confidence: 0.9, TurnRange: [2]int{2, 4}},
		{Topic: "y", Confidence: 0.9, TurnRange: [2]int{0, 0}}, // no range
	}}
	a := NewAnalyzer(m, analyzeCfg())
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{msgs})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
	if got[0].TurnRange != [2]int{1, 4} {
		t.Errorf("mapped turn_range = %v, want [1 4] (rendered turns 2..4 → global 1..4)", got[0].TurnRange)
	}
	if got[1].TurnRange != [2]int{0, 0} {
		t.Errorf("zero turn_range should stay [0 0], got %v", got[1].TurnRange)
	}
}

func TestDedupe(t *testing.T) {
	in := []Candidate{
		{Topic: "PostgreSQL", Confidence: 0.9, Tags: []string{"db"}, TurnRange: [2]int{1, 2}},
		{Topic: "postgresql", Confidence: 0.7, Tags: []string{"db", "sql"}, TurnRange: [2]int{3, 4}},
		{Topic: "Go 并发", Confidence: 0.8, TurnRange: [2]int{2, 2}},
	}
	got := dedupe(in)
	if len(got) != 2 {
		t.Fatalf("dedupe = %d, want 2 (postgresql merged)", len(got))
	}
	first := got[0]
	if first.Confidence != 0.9 {
		t.Errorf("merged confidence = %f, want 0.9 (highest)", first.Confidence)
	}
	if first.TurnRange != [2]int{1, 4} {
		t.Errorf("merged turn_range = %v, want [1 4]", first.TurnRange)
	}
	if len(first.Tags) < 2 {
		t.Errorf("merged tags = %v, want combined", first.Tags)
	}
}

func TestAnalyzeDedupesAcrossWindows(t *testing.T) {
	// Same topic surfaced by every prefix window merges into one candidate.
	m := &mockLLM{cands: []Candidate{{Topic: "dup", Confidence: 0.9}}}
	a := NewAnalyzer(m, analyzeCfg())
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{
		turnsFrom("one"),
		turnsFrom("two"),
		turnsFrom("three"),
	})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1 (deduped across windows)", len(got))
	}
	if m.callN != 3 {
		t.Errorf("LLM calls = %d, want 3 (all windows still analyzed)", m.callN)
	}
}

func TestAnalyzeFiltersLowConfidence(t *testing.T) {
	m := &mockLLM{cands: []Candidate{
		{Topic: "keep", Confidence: 0.9},
		{Topic: "drop-low", Confidence: 0.2},
		{Topic: "keep-edge", Confidence: 0.3}, // boundary ≥ min(0.3)
	}}
	a := NewAnalyzer(m, analyzeCfg())
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
	m := &mockLLM{perCall: [][]Candidate{
		{{Topic: "w1", Confidence: 0.9}},
		{{Topic: "w2", Confidence: 0.9}},
		{{Topic: "w3", Confidence: 0.9}},
	}}
	a := NewAnalyzer(m, analyzeCfg())
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
	// max_candidates_per_window=1 → each window keeps at most its top
	// candidate. Distinct topics per window so dedupe does not merge them.
	// Concurrent consumption order of perCall is not guaranteed, so assert on
	// the candidate count and set rather than window↔topic mapping.
	m := &mockLLM{perCall: [][]Candidate{
		{{Topic: "a", Confidence: 0.9}, {Topic: "a2", Confidence: 0.8}},
		{{Topic: "b", Confidence: 0.9}, {Topic: "b2", Confidence: 0.8}},
	}}
	cfg := analyzeCfg()
	cfg.MaxCandidates = 1
	a := NewAnalyzer(m, cfg)
	got, err := a.Analyze(context.Background(), "agent-a", [][]llm.Message{
		turnsFrom("one"),
		turnsFrom("two"),
	})
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	// Without the per-window cap each window would contribute a+a2 / b+b2
	// (4 distinct topics); with the cap exactly the top 1 of each survives.
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 (one per window, capped at 1 each)", len(got))
	}
	set := map[string]bool{}
	for _, c := range got {
		set[c.Topic] = true
	}
	if len(set) != 2 {
		t.Errorf("topics = %v, want exactly 2 distinct", set)
	}
	if m.callN != 2 {
		t.Errorf("LLM calls = %d, want 2 (no early stop across windows)", m.callN)
	}
}

func TestAnalyzeSkipsEmptyWindow(t *testing.T) {
	m := &mockLLM{cands: []Candidate{{Topic: "x", Confidence: 0.9}}}
	a := NewAnalyzer(m, analyzeCfg())
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
	a := NewAnalyzer(m, analyzeCfg())
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
