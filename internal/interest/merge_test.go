package interest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/djasdh/interest-memory/internal/fork"
	"github.com/djasdh/interest-memory/internal/llm"
)

// mapEmbedder returns per-text vectors (embedding keyed by the full text that
// DedupeMerge submits: "Topic\nReason").
type mapEmbedder struct {
	vecs   map[string][]float32
	misses int
}

func (m *mapEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := m.vecs[text]; ok {
		return v, nil
	}
	m.misses++
	return []float32{0, 0}, nil
}

// fakeClusterLLM records calls and replies with a fixed verdict array.
type fakeClusterLLM struct {
	calls  int
	groups []mergeVerdict
}

func (f *fakeClusterLLM) ChatJSON(_ context.Context, _ []llm.Message, out any) error {
	f.calls++
	b, err := json.Marshal(f.groups)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func mergeCand(topic string, conf float64) fork.Candidate {
	return fork.Candidate{Topic: topic, Reason: "reason for " + topic, Confidence: conf, Tags: []string{"t"}}
}

func TestDedupeMergeStringDedupNoLLM(t *testing.T) {
	em := &mapEmbedder{vecs: map[string][]float32{
		"Go 泛型\nreason for Go 泛型":         {1, 0},
		"  go  泛型 \nreason for   go  泛型 ": {1, 0},
	}}
	lm := &fakeClusterLLM{groups: nil}
	out, err := DedupeMerge(context.Background(), "ag", em, lm, 0.6, []fork.Candidate{
		mergeCand("Go 泛型", 0.9),
		mergeCand("  go  泛型 ", 0.8),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("deduped = %d, want 1", len(out))
	}
	if out[0].Candidate.Topic != "Go 泛型" {
		t.Errorf("kept topic = %q, want the higher-confidence original", out[0].Candidate.Topic)
	}
	if out[0].Candidate.Confidence != 0.9 {
		t.Errorf("kept confidence = %f, want 0.9", out[0].Candidate.Confidence)
	}
	if lm.calls != 0 {
		t.Errorf("LLM calls = %d, want 0 (identical topics dedupe without LLM)", lm.calls)
	}
}

func TestDedupeMergeClustersBySimilarity(t *testing.T) {
	// A/B near (cos ~1), C far (cos ~0). Expect one cluster → one LLM call
	// covering A and B; C stays isolated (no LLM call).
	em := &mapEmbedder{vecs: map[string][]float32{
		"A\nreason for A": {1, 0},
		"B\nreason for B": {0.9, 0.1},
		"C\nreason for C": {0, 1},
	}}
	lm := &fakeClusterLLM{groups: []mergeVerdict{
		{Action: "keep", SourceTopics: []string{"A", "B"}},
	}}
	out, err := DedupeMerge(context.Background(), "ag", em, lm, 0.6, []fork.Candidate{
		mergeCand("A", 0.9),
		mergeCand("B", 0.85),
		mergeCand("C", 0.8),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lm.calls != 1 {
		t.Errorf("LLM calls = %d, want 1 (one cluster only)", lm.calls)
	}
	if len(out) != 3 {
		t.Fatalf("out = %d, want 3 (A+B kept + C)", len(out))
	}
}

func TestDedupeMergeLLMMergesCluster(t *testing.T) {
	em := &mapEmbedder{vecs: map[string][]float32{
		"A\nreason for A": {1, 0},
		"B\nreason for B": {0.9, 0.1},
	}}
	lm := &fakeClusterLLM{groups: []mergeVerdict{
		{
			Action:       "merge",
			SourceTopics: []string{"A", "B"},
			Merged: mergeCandidate{
				Topic: "A合并B", Reason: "merged", Confidence: 0.95,
				Tags: []string{"t"},
			},
		},
	}}
	out, err := DedupeMerge(context.Background(), "ag", em, lm, 0.6, []fork.Candidate{
		mergeCand("A", 0.9),
		mergeCand("B", 0.85),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %d, want 1 (merged)", len(out))
	}
	if out[0].Candidate.Topic != "A合并B" {
		t.Errorf("merged topic = %q, want A合并B", out[0].Candidate.Topic)
	}
	if len(out[0].Vec) == 0 {
		t.Error("merged point missing EBD vector")
	}
}

func TestDedupeMergeKeepsSeparateCandidates(t *testing.T) {
	em := &mapEmbedder{vecs: map[string][]float32{
		"A\nreason for A": {1, 0},
		"B\nreason for B": {0.9, 0.1},
		"C\nreason for C": {0, 1},
	}}
	// Cluster {A,B}: LLM says compose into a new topic; C isolated.
	lm := &fakeClusterLLM{groups: []mergeVerdict{
		{
			Action:       "compose",
			SourceTopics: []string{"A", "B"},
			Merged: mergeCandidate{
				Topic: "A与B合成", Reason: "composed", Confidence: 0.9,
			},
		},
	}}
	out, err := DedupeMerge(context.Background(), "ag", em, lm, 0.6, []fork.Candidate{
		mergeCand("A", 0.9),
		mergeCand("B", 0.85),
		mergeCand("C", 0.8),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %d, want 2 (composed + C)", len(out))
	}
	if out[0].Candidate.Topic != "A与B合成" {
		t.Errorf("composed topic = %q, want A与B合成", out[0].Candidate.Topic)
	}
	if out[1].Candidate.Topic != "C" {
		t.Errorf("second topic = %q, want C", out[1].Candidate.Topic)
	}
}
