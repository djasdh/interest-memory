package interest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/store"
)

// verdictLLM returns scripted per-call JSON verdicts (array of any shapes,
// marshaled into the ChatJSON out param).
type verdictLLM struct {
	calls   int
	results []any
	prompts []string
}

func (f *verdictLLM) ChatJSON(_ context.Context, msgs []llm.Message, out any) error {
	f.calls++
	if f.calls > len(f.results) {
		// Return empty when script exhausted.
		return json.Unmarshal([]byte(`{}`), out)
	}
	f.prompts = append(f.prompts, msgs[0].Content)
	b, err := json.Marshal(f.results[f.calls-1])
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// recordingEmbedder returns distinct vectors per text and records texts.
type recordingEmbedder struct {
	vecs   map[string][]float32
	texts  []string
	nextID int
}

func (m *recordingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	m.texts = append(m.texts, text)
	if v, ok := m.vecs[text]; ok {
		return v, nil
	}
	m.nextID++
	return []float32{float32(m.nextID), 0}, nil
}

func decisionLLM(topic, action, target string, merged mergeCandidate) map[string]any {
	return map[string]any{
		"source_topic": topic,
		"action":       action,
		"target_id":    target,
		"merged":       merged,
	}
}

// decisionLLMUpdates builds a decision with associated historical updates.
func decisionLLMUpdates(topic, action, target string, merged mergeCandidate, updates []map[string]any) map[string]any {
	d := decisionLLM(topic, action, target, merged)
	if len(updates) > 0 {
		d["updates"] = updates
	}
	return d
}

// buildComponent builds a Component with one member and one hist point.
func buildComponent(member string, histPt HistPoint) Component {
	c := Component{
		Members:    []Point{{Candidate: mergeCand(member, 0.9), Vec: []float32{1, 0}}},
		Hist:       []HistPoint{histPt},
		MemberHist: map[string][]HistPoint{member: {histPt}},
	}
	return c
}

func histPtComponent(id, name string) HistPoint {
	return HistPoint{
		Pt:  store.InterestPoint{ID: id, AgentID: "ag", Name: name, Status: "active"},
		Vec: []float32{0.9, 0.1},
	}
}

func TestAdjudicateMergeKeepsHistID(t *testing.T) {
	h := histPtComponent("h1", "历史点")
	comp := buildComponent("a", h)
	res := ClusterResult{Components: []Component{comp}}

	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLM("a", "merge", "h1", mergeCandidate{
				Topic: "合并后", Reason: "r", Confidence: 0.95, Tags: []string{"t"},
			}),
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FinalPoints) != 1 {
		t.Fatalf("final points = %d, want 1 (merged into h1)", len(out.FinalPoints))
	}
	fp := out.FinalPoints[0]
	if fp.Point.ID != "h1" {
		t.Errorf("final id = %q, want h1 (historical id preserved)", fp.Point.ID)
	}
	if fp.Action != "update" {
		t.Errorf("action = %q, want update", fp.Action)
	}
	if len(out.Archived) != 0 {
		t.Errorf("archived = %+v, want none", out.Archived)
	}
}

func TestAdjudicateKeepUpdatesHist(t *testing.T) {
	// keep + 带动 update（Go 1.19 例子）：a 新建 + h1 更新，两者都存活。
	h := histPtComponent("h1", "历史点")
	comp := buildComponent("a", h)
	res := ClusterResult{Components: []Component{comp}}

	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLMUpdates("a", "keep", "", mergeCandidate{
				Topic: "新点", Reason: "r", Confidence: 0.9, Tags: []string{},
			}, []map[string]any{
				{"target_id": "h1", "merged": mergeCandidate{
					Topic: "历史点更新", Reason: "upd", Confidence: 0.8, Tags: []string{},
				}},
			}),
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FinalPoints) != 2 {
		t.Fatalf("final points = %d, want 2 (a new + h1 updated)", len(out.FinalPoints))
	}
	ids := map[string]bool{}
	actions := map[string]string{}
	for _, fp := range out.FinalPoints {
		ids[fp.Point.ID] = true
		actions[fp.Point.ID] = fp.Action
	}
	if !ids["h1"] {
		t.Errorf("missing updated h1; ids = %v", ids)
	}
	if actions["h1"] != "update" {
		t.Errorf("h1 action = %q, want update", actions["h1"])
	}
	// a's new point: not h1, action create.
	var aID string
	for _, fp := range out.FinalPoints {
		if fp.Point.ID != "h1" {
			aID = fp.Point.ID
			if fp.Action != "create" {
				t.Errorf("a action = %q, want create", fp.Action)
			}
		}
	}
	if aID == "" {
		t.Fatal("missing a new point")
	}
}

func TestAdjudicateKeepCreatesNewHistUntouched(t *testing.T) {
	h := histPtComponent("h1", "历史点")
	comp := buildComponent("a", h)
	res := ClusterResult{Components: []Component{comp}}

	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLM("a", "keep", "", mergeCandidate{Topic: "独立点", Reason: "r", Confidence: 0.8}),
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FinalPoints) != 1 {
		t.Fatalf("final points = %d, want 1 (new point only)", len(out.FinalPoints))
	}
	fp := out.FinalPoints[0]
	if fp.Action != "create" {
		t.Errorf("action = %q, want create", fp.Action)
	}
	if fp.Point.ID == "h1" {
		t.Error("new point must not reuse the historical id")
	}
}

func TestAdjudicateKeepFallsBackToMemberTags(t *testing.T) {
	// keep 时 LLM merged 给空 tags → Keywords 回退到该成员原始候选 Tags。
	h := histPtComponent("h1", "历史点")
	comp := buildComponent("a", h)
	res := ClusterResult{Components: []Component{comp}}

	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLM("a", "keep", "", mergeCandidate{Topic: "独立点", Reason: "r", Confidence: 0.8, Tags: []string{}}),
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FinalPoints) != 1 {
		t.Fatalf("final = %d, want 1", len(out.FinalPoints))
	}
	fp := out.FinalPoints[0]
	// 原始候选 mergeCand("a", 0.9) 带 Tags: ["t"]。
	if len(fp.Point.Keywords) != 1 || fp.Point.Keywords[0] != "t" {
		t.Errorf("keywords = %v, want fallback [t] (member candidate tags)", fp.Point.Keywords)
	}
}

func TestAdjudicateMergeFallsBackToMemberTags(t *testing.T) {
	// merge 时 LLM merged 给空 tags → 历史点 Keywords 回退到成员原始 Tags。
	h := histPtComponent("h1", "历史点")
	comp := buildComponent("a", h)
	res := ClusterResult{Components: []Component{comp}}

	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLM("a", "merge", "h1", mergeCandidate{Topic: "合并点", Reason: "r", Confidence: 0.9, Tags: []string{}}),
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FinalPoints) != 1 {
		t.Fatalf("final = %d, want 1", len(out.FinalPoints))
	}
	fp := out.FinalPoints[0]
	if len(fp.Point.Keywords) != 1 || fp.Point.Keywords[0] != "t" {
		t.Errorf("keywords = %v, want fallback [t] (member candidate tags)", fp.Point.Keywords)
	}
}

func TestAdjudicateArchiveArchivesHist(t *testing.T) {
	h := histPtComponent("h1", "历史点")
	comp := buildComponent("a", h)
	res := ClusterResult{Components: []Component{comp}}

	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLM("a", "archive", "h1", mergeCandidate{Topic: "推翻点", Reason: "r", Confidence: 0.9}),
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Archived) != 1 || out.Archived[0].Pt.ID != "h1" {
		t.Errorf("archived = %+v, want [h1]", out.Archived)
	}
	if len(out.FinalPoints) != 1 || out.FinalPoints[0].Action != "create" {
		t.Errorf("final = %+v, want one create", out.FinalPoints)
	}
}

func TestAdjudicateOmissionVoidsComponent(t *testing.T) {
	// Component {a, b} + h1. LLM only adjudicates a, omits b → component
	// voided: h1 untouched, both a and b become new points, no contradiction.
	h := histPtComponent("h1", "历史点")
	comp := Component{
		Members: []Point{
			{Candidate: mergeCand("a", 0.9), Vec: []float32{1, 0}},
			{Candidate: mergeCand("b", 0.85), Vec: []float32{0.9, 0.1}},
		},
		Hist: []HistPoint{h},
		MemberHist: map[string][]HistPoint{
			"a": {h},
			"b": {h},
		},
	}
	res := ClusterResult{Components: []Component{comp}}
	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLM("a", "merge", "h1", mergeCandidate{Topic: "x", Reason: "r", Confidence: 0.9}),
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Voided: h1 not in FinalPoints (not updated), a and b both created.
	if len(out.FinalPoints) != 2 {
		t.Fatalf("final points = %d, want 2 (a and b new)", len(out.FinalPoints))
	}
	for _, fp := range out.FinalPoints {
		if fp.Point.ID == "h1" {
			t.Error("h1 must not be touched when a member is omitted")
		}
		if fp.Action != "create" {
			t.Errorf("action = %q, want create", fp.Action)
		}
	}
}

func TestAdjudicateIsolatedMeta(t *testing.T) {
	iso := Point{Candidate: mergeCand("孤立点", 0.9), Vec: []float32{1, 0}}
	res := ClusterResult{Isolated: []Point{iso}}
	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"meta": map[string]any{
			"subjective": true, "reliability_status": "supported",
			"confidence": 0.85, "freshness_level": "fresh", "ttl_days": 30,
			"wiki_worthy": true,
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FinalPoints) != 1 {
		t.Fatalf("final = %d, want 1 (isolated created)", len(out.FinalPoints))
	}
	fp := out.FinalPoints[0]
	if fp.Point.ID == "" || fp.Action != "create" {
		t.Errorf("isolated final = %+v, want create with id", fp)
	}
	if !fp.Point.Subjective {
		t.Error("isolated meta subjective not applied")
	}
	if fp.Point.Reliability.Status != "supported" || fp.Point.Reliability.Confidence != 0.85 {
		t.Errorf("isolated reliability = %+v, want supported/0.85", fp.Point.Reliability)
	}
	if fp.Point.Freshness.Level != "fresh" || fp.Point.Freshness.TTLDays != 30 {
		t.Errorf("isolated freshness = %+v, want fresh/30", fp.Point.Freshness)
	}
}

func TestAdjudicateContradictions(t *testing.T) {
	h := histPtComponent("h1", "历史点")
	comp := buildComponent("a", h)
	res := ClusterResult{Components: []Component{comp}}
	em := &recordingEmbedder{}
	lm := &verdictLLM{results: []any{map[string]any{
		"decisions": []map[string]any{
			decisionLLM("a", "keep", "", mergeCandidate{Topic: "a", Reason: "r", Confidence: 0.8}),
		},
		"contradictions": []map[string]any{
			{"left": "a", "right": "h1", "description": "矛盾"},
		},
	}}}
	out, err := Adjudicate(context.Background(), "ag", em, lm, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contradictions) != 1 {
		t.Fatalf("contradictions = %d, want 1", len(out.Contradictions))
	}
	if out.Contradictions[0].Description != "矛盾" {
		t.Errorf("contradiction = %+v", out.Contradictions[0])
	}
}

// failingLLM always errors — simulates an exhausted/network-failed call.
type failingLLM struct{}

func (failingLLM) ChatJSON(context.Context, []llm.Message, any) error { return fmt.Errorf("llm down") }

func TestAdjudicateComponentFailureVoids(t *testing.T) {
	// LLM failure on the component → voided: h1 untouched, members created,
	// no contradictions.
	h := histPtComponent("h1", "历史点")
	comp := Component{
		Members:    []Point{{Candidate: mergeCand("a", 0.9), Vec: []float32{1, 0}}},
		Hist:       []HistPoint{h},
		MemberHist: map[string][]HistPoint{"a": {h}},
	}
	res := ClusterResult{Components: []Component{comp}}
	out, err := Adjudicate(context.Background(), "ag", nil, failingLLM{}, res, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FinalPoints) != 1 {
		t.Fatalf("final = %d, want 1 (member created despite failure)", len(out.FinalPoints))
	}
	if out.FinalPoints[0].Point.ID == "h1" {
		t.Error("h1 must not be touched on component failure")
	}
	if len(out.Archived) != 0 {
		t.Errorf("archived = %+v, want none", out.Archived)
	}
	if len(out.Contradictions) != 0 {
		t.Errorf("contradictions = %+v, want none (voided)", out.Contradictions)
	}
}
