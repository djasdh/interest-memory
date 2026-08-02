package fork

import (
	"context"
	"fmt"
	"strings"

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
// a side LLM call per window (design §五 step 1).
type Analyzer struct {
	llm           LLM
	windowTurns   int
	maxCandidates int
	minConfidence float64
}

// NewAnalyzer builds an Analyzer from fork config.
func NewAnalyzer(client LLM, cfg config.ForkConfig) *Analyzer {
	if cfg.WindowTurns <= 0 {
		cfg.WindowTurns = 10
	}
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = 0.3
	}
	return &Analyzer{
		llm:           client,
		windowTurns:   cfg.WindowTurns,
		maxCandidates: cfg.MaxCandidates,
		minConfidence: cfg.MinConfidence,
	}
}

// SplitWindows slices turns into fixed-turn windows (design §五: 按 turn_count
// 切固定轮数窗口). Empty or non-positive windowTurns falls back to 10.
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

// Analyze extracts candidates from each window and merges them. Candidate
// count is capped per window by max_candidates_per_window (config semantics).
func (a *Analyzer) Analyze(ctx context.Context, agentID string, windows [][]llm.Message) ([]Candidate, error) {
	var out []Candidate
	for _, w := range windows {
		cands, err := a.extract(ctx, w)
		if err != nil {
			return out, fmt.Errorf("fork: analyze: %w", err)
		}
		out = append(out, cands...)
	}
	return out, nil
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

Return a JSON array of objects, each with:
  - "topic": short phrase describing the topic
  - "reason": why this is worth remembering (1 sentence)
  - "confidence": 0.0 to 1.0
  - "tags": array of short tags (max 5)
  - "turn_range": [start_turn, end_turn] (approximate turn numbers from the excerpt)

If nothing is worth remembering, return an empty array [].

Conversation excerpt:
%s

Return ONLY valid JSON, no other text.`, snapshot)

	var cands []Candidate
	if err := a.llm.ChatJSON(ctx, []llm.Message{{Role: "user", Content: prompt}}, &cands); err != nil {
		return nil, err
	}
	filtered := cands[:0]
	for _, c := range cands {
		if c.Confidence >= a.minConfidence {
			filtered = append(filtered, c)
		}
	}
	if a.maxCandidates > 0 && len(filtered) > a.maxCandidates {
		filtered = filtered[:a.maxCandidates]
	}
	return filtered, nil
}

// summarize renders the most relevant parts of a window for the extraction
// prompt: user/assistant text content, numbered as turns (adaptation of
// my-agent-core's summarizeMessagesForInterest to llm.Message).
func summarize(turns []llm.Message) string {
	start := 0
	if len(turns) > 20 {
		start = len(turns) - 20
	}
	var b strings.Builder
	turnNum := 1
	for _, m := range turns[start:] {
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
