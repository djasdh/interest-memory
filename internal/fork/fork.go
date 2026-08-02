package fork

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"interest-memory/internal/config"
	"interest-memory/internal/llm"
)

// Candidate is one interest point extracted from a conversation window.
// Aligned with my-agent-core's InterestPoint shape (topic/reason/confidence/
// tags/turn_range).
type Candidate struct {
	Topic      string   `json:"topic"`
	Reason     string   `json:"reason"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	TurnRange  [2]int   `json:"turn_range"` // [start_turn, end_turn] 1-indexed
	Subjective bool     `json:"subjective"` // 主观观点/偏好（豁免 verify 联网核查）
}

// LLM is the chat surface fork needs (implemented by *llm.Client).
// Kept as a narrow interface so tests can inject a fake.
type LLM interface {
	ChatJSON(ctx context.Context, messages []llm.Message, out any) error
}

// ForkAnalyzer is the domain interface (design §七). The service layer
// depends on this, not on the concrete Analyzer.
type ForkAnalyzer interface {
	Analyze(ctx context.Context, agentID string, windows [][]llm.Message) ([]Candidate, error)
}

// Analyzer extracts candidate interest points from transcript windows using
// a side LLM call per window (design §五 step 1). Windows are analyzed
// concurrently (bounded by maxConcurrency); results are deduplicated across
// the overlapping prefix windows.
type Analyzer struct {
	llm           LLM
	prefixStep    int
	maxWindows    int
	maxConcurrency int
	maxCandidates int
	minConfidence float64
}

// NewAnalyzer builds an Analyzer from fork config.
func NewAnalyzer(client LLM, cfg config.ForkConfig) *Analyzer {
	if cfg.PrefixStep <= 0 {
		cfg.PrefixStep = 5
	}
	if cfg.MaxWindows <= 0 {
		cfg.MaxWindows = 8
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = 0.3
	}
	return &Analyzer{
		llm:            client,
		prefixStep:     cfg.PrefixStep,
		maxWindows:     cfg.MaxWindows,
		maxConcurrency: cfg.MaxConcurrency,
		maxCandidates:  cfg.MaxCandidates,
		minConfidence:  cfg.MinConfidence,
	}
}

// SplitWindows slices turns into fixed-turn windows (design §五: 按 turn_count
// 切固定轮数窗口). Empty or non-positive windowTurns falls back to 10.
// Kept for compatibility; production uses SplitPrefixWindows.
func SplitWindows(turns []llm.Message, windowTurns int) [][]llm.Message {
	if windowTurns <= 0 {
		windowTurns = 10
	}
	if len(turns) == 0 {
		return nil
	}
	var out [][]llm.Message
	for i := 0; i < len(turns); i += windowTurns {
		end := i + windowTurns
		if end > len(turns) {
			end = len(turns)
		}
		out = append(out, turns[i:end])
	}
	return out
}

// SplitPrefixWindows slices turns into growing prefix windows, stepping one
// boundary per userStep user turns. Rationale: the rendered prompt of window
// k is a strict string prefix of window k+1, so LLM providers with prompt
// prefix caching (DeepSeek / SiliconFlow context caching) hit the shared
// prefix and cut token cost. When the transcript has fewer than userStep
// user turns it returns a single full window (no split — straight into the
// extraction/verification flow).
//
// maxWindows>0 caps the result by keeping the longest windows (they remain a
// prefix chain and the longest covers the full transcript).
func SplitPrefixWindows(turns []llm.Message, userStep, maxWindows int) [][]llm.Message {
	if userStep <= 0 {
		userStep = 5
	}
	if len(turns) == 0 {
		return nil
	}
	// Positions of user messages (user-turn counting).
	var pos []int
	for i, m := range turns {
		if m.Role == "user" {
			pos = append(pos, i)
		}
	}
	if len(pos) < userStep {
		return [][]llm.Message{turns}
	}
	var out [][]llm.Message
	for k := userStep; k <= len(pos); k += userStep {
		end := pos[k-1] + 1
		w := turns[0:end]
		if len(out) == 0 || !sameWindow(out[len(out)-1], w) {
			out = append(out, w)
		}
	}
	// Full window last: covers trailing turns after the last step boundary.
	if len(out) == 0 || !sameWindow(out[len(out)-1], turns) {
		out = append(out, turns)
	}
	if maxWindows > 0 && len(out) > maxWindows {
		out = out[len(out)-maxWindows:]
	}
	return out
}

func sameWindow(a, b []llm.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Analyze extracts candidates from each window concurrently (bounded by
// maxConcurrency) and merges them in window order. Candidate count is capped
// per window by max_candidates_per_window (config semantics); overlapping
// prefix windows are deduplicated afterwards.
func (a *Analyzer) Analyze(ctx context.Context, agentID string, windows [][]llm.Message) ([]Candidate, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	results := make([]result, len(windows))
	sem := make(chan struct{}, a.maxConcurrency)
	var wg sync.WaitGroup
	for i, w := range windows {
		wg.Add(1)
		go func(i int, w []llm.Message) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cands, err := a.extract(ctx, w)
			results[i] = result{cands: cands, err: err}
		}(i, w)
	}
	wg.Wait()

	var out []Candidate
	for _, r := range results {
		if r.err != nil {
			return out, fmt.Errorf("fork: analyze: %w", r.err)
		}
		out = append(out, r.cands...)
	}
	return dedupe(out), nil
}

type result struct {
	cands []Candidate
	err   error
}

// extract asks the side LLM to identify interest points in a single window,
// filters low-confidence results (design: confidence≥0.3 过滤) and caps the
// window at max_candidates_per_window.
func (a *Analyzer) extract(ctx context.Context, turns []llm.Message) ([]Candidate, error) {
	snapshot := summarize(turns)
	if snapshot == "" {
		return nil, nil
	}
	prompt := fmt.Sprintf(`Analyse this conversation excerpt and identify topics that would be worth remembering for future conversations. 
These could be:
- Technical decisions or architectural choices made
- User preferences or coding style preferences
- Project-specific conventions or constraints
- Important facts about the codebase
- Any strong opinions expressed

For each topic, judge whether it is subjective (the user's own preference, taste, or opinion — e.g. "I prefer Go over Rust") or objective (a factual claim about the world — e.g. "PostgreSQL supports JSONB").

Return a JSON array of objects, each with:
  - "topic": short phrase describing the topic
  - "reason": why this is worth remembering (1 sentence)
  - "confidence": 0.0 to 1.0
  - "tags": array of short tags (max 5)
  - "turn_range": [start_turn, end_turn] (approximate turn numbers from the excerpt)
  - "subjective": true if this is a subjective preference/opinion, false if objective

If nothing is worth remembering, return an empty array [].

Conversation excerpt:
%s

Return ONLY valid JSON, no other text.`, snapshot)

	var cands []Candidate
	if err := a.llm.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &cands); err != nil {
		return nil, err
	}
	// Copy-filter into a fresh slice: cands may be shared across concurrent
	// window goroutines (e.g. test fakes returning a common slice).
	var filtered []Candidate
	for _, c := range cands {
		if c.Confidence >= a.minConfidence {
			filtered = append(filtered, mapTurnRange(c, renderIndices(turns)))
		}
	}
	if a.maxCandidates > 0 && len(filtered) > a.maxCandidates {
		filtered = filtered[:a.maxCandidates]
	}
	return filtered, nil
}

// renderIndices returns the global message indexes that summarize() renders,
// in order — i.e. non-empty user/assistant messages. The extraction prompt's
// turn numbers are 1-based positions in this sequence; candidates' TurnRange
// must be mapped back to global indexes so downstream code can slice the
// transcript for the exact conversation segment.
func renderIndices(turns []llm.Message) []int {
	var idx []int
	for i, m := range turns {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		idx = append(idx, i)
	}
	return idx
}

// mapTurnRange converts a candidate's window-local TurnRange (1-based rendered
// turn numbers) into global message indexes. A zero range stays zero.
func mapTurnRange(c Candidate, idx []int) Candidate {
	if len(idx) == 0 || c.TurnRange == [2]int{0, 0} {
		return c
	}
	s, e := c.TurnRange[0], c.TurnRange[1]
	if s < 1 {
		s = 1
	}
	if e > len(idx) {
		e = len(idx)
	}
	if s > len(idx) {
		return c
	}
	start := idx[s-1]
	end := idx[e-1]
	c.TurnRange = [2]int{start, end}
	return c
}

// dedupe merges candidates that describe the same topic (case/whitespace
// normalized) — prefix windows surface the same point repeatedly. Keeps the
// highest-confidence variant, merges turn_range to the span, and folds tags.
func dedupe(cands []Candidate) []Candidate {
	var out []Candidate
	for _, c := range cands {
		key := normalizeTopic(c.Topic)
		if key == "" {
			out = append(out, c)
			continue
		}
		idx := -1
		for i := range out {
			if normalizeTopic(out[i].Topic) == key {
				idx = i
				break
			}
		}
		if idx == -1 {
			out = append(out, c)
			continue
		}
		merged := out[idx]
		if c.Confidence > merged.Confidence {
			merged.Topic = c.Topic
			merged.Reason = c.Reason
			merged.Subjective = c.Subjective
			merged.Confidence = c.Confidence
		}
		if c.TurnRange[0] > 0 && (merged.TurnRange[0] == 0 || c.TurnRange[0] < merged.TurnRange[0]) {
			merged.TurnRange[0] = c.TurnRange[0]
		}
		if c.TurnRange[1] > merged.TurnRange[1] {
			merged.TurnRange[1] = c.TurnRange[1]
		}
		for _, tag := range c.Tags {
			if !containsString(merged.Tags, tag) {
				merged.Tags = append(merged.Tags, tag)
			}
		}
		out[idx] = merged
	}
	return out
}

func normalizeTopic(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// summarize renders the window for the extraction prompt: user/assistant
// text content, numbered as turns from 1 (adaptation of my-agent-core's
// summarizeMessagesForInterest to llm.Message).
//
// The WHOLE window is rendered (no tail truncation) so that for prefix
// windows the rendered text of window k is a strict prefix of window k+1 —
// required for LLM provider prompt prefix caching to hit.
func summarize(turns []llm.Message) string {
	var b strings.Builder
	turnNum := 1
	for _, m := range turns {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		switch m.Role {
		case "user":
			b.WriteString(fmt.Sprintf("Turn %d [USER]: %s\n", turnNum, text))
		case "assistant":
			b.WriteString(fmt.Sprintf("Turn %d [ASSISTANT]: %s\n", turnNum, text))
		default:
			continue // system / tool roles are not interest-bearing
		}
		turnNum++
	}
	return b.String()
}

var _ ForkAnalyzer = (*Analyzer)(nil)
