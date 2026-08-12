package verify

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djasdh/interest-memory/internal/fork"
	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/websearch"
)

// fakeLLM returns a canned JSON per call, optionally erroring. Thread-safe:
// VerifyCandidates runs candidates concurrently.
type fakeLLM struct {
	responses []any // one per ChatJSON call, consumed in order
	idx       int
	err       error
	mu        chan struct{}
	prompts   []string
	pmu       sync.Mutex
}

func (f *fakeLLM) next() []byte {
	if f.mu != nil {
		<-f.mu
		defer func() { f.mu <- struct{}{} }()
	}
	var payload []byte
	if f.idx < len(f.responses) {
		payload, _ = json.Marshal(f.responses[f.idx])
		f.idx++
	} else {
		payload = []byte("{}")
	}
	return payload
}

func (f *fakeLLM) ChatJSON(_ context.Context, msgs []llm.Message, out any) error {
	if f.err != nil {
		return f.err
	}
	if len(msgs) > 0 {
		f.pmu.Lock()
		f.prompts = append(f.prompts, msgs[0].Content)
		f.pmu.Unlock()
	}
	return json.Unmarshal(f.next(), out)
}

func (f *fakeLLM) callCount() int {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return len(f.prompts)
}

func (f *fakeLLM) lastPrompt() string {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

func newSerialFakeLLM(responses []any) *fakeLLM {
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	return &fakeLLM{responses: responses, mu: mu}
}

type fakeSearcher struct {
	items []websearch.SearchItem
	err   error
	calls int
}

func (f *fakeSearcher) Search(_ context.Context, _ string, _ int) ([]websearch.SearchItem, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

type fakeRetriever struct {
	hits []vec.Hit
	err  error
}

func (f *fakeRetriever) Search(_ context.Context, _ string, _ []float32, _ int) ([]vec.Hit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

type fakeEmbedder struct {
	v       []float32
	err     error
	calls   int
	batchFn func([]string) [][]float32
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.v == nil {
		return make([]float32, 8), nil
	}
	return f.v, nil
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.batchFn != nil {
		return f.batchFn(texts), nil
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 8)
	}
	return out, nil
}

type fakeStore struct {
	ip   *store.InterestPoint
	page *store.Page
}

func (f *fakeStore) GetInterestPoint(_ context.Context, _, _ string) (*store.InterestPoint, error) {
	return f.ip, nil
}
func (f *fakeStore) UpsertInterestPoint(_ context.Context, p store.InterestPoint) error {
	f.ip = &p
	return nil
}
func (f *fakeStore) GetPage(_ context.Context, _, _ string) (*store.Page, error) {
	return f.page, nil
}
func (f *fakeStore) ListClaims(_ context.Context, _, _ string) ([]store.Claim, error) {
	if f.page == nil {
		return nil, nil
	}
	return f.page.Claims, nil
}
func (f *fakeStore) ResolveReplacement(context.Context, string, string) (*store.Replacement, error) {
	return nil, nil
}

func cands(n int) []fork.Candidate {
	out := make([]fork.Candidate, n)
	for i := range out {
		out[i] = fork.Candidate{Topic: "topic", Reason: "reason", Confidence: 0.9}
	}
	return out
}

func TestVerifyCandidatesSupported(t *testing.T) {
	f := newSerialFakeLLM([]any{map[string]any{
		"supported":       true,
		"confidence":      0.9,
		"status":          "supported",
		"evidence":        []string{"matches known facts"},
		"freshness_level": "fresh",
		"ttl_days":        90,
	}})
	se := &fakeSearcher{items: []websearch.SearchItem{{Title: "t", URL: "u", Snippet: "s"}}}
	v := New(f, &fakeStore{}, se, nil, nil, Config{UseWebSearch: true, SearchMax: 5})
	out, err := v.VerifyCandidates(context.Background(), "agent-a", cands(1))
	if err != nil {
		t.Fatalf("VerifyCandidates error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("verified = %d, want 1", len(out))
	}
	got := out[0]
	if got.Reliability.Status != "supported" || got.Reliability.Confidence != 0.9 {
		t.Errorf("reliability = %+v", got.Reliability)
	}
	if got.Freshness.Level != "fresh" {
		t.Errorf("freshness level = %s", got.Freshness.Level)
	}
	if len(got.Reliability.Evidence) == 0 || got.Reliability.Evidence[0].Kind != "web" {
		t.Errorf("expected web evidence, got %+v", got.Reliability.Evidence)
	}
}

func TestVerifyCandidatesDegradesOnLLMError(t *testing.T) {
	f := newSerialFakeLLM(nil)
	f.err = context.DeadlineExceeded
	v := New(f, &fakeStore{}, nil, nil, nil, Config{UseWebSearch: false})
	out, err := v.VerifyCandidates(context.Background(), "agent-a", cands(2))
	if err != nil {
		t.Fatalf("expected no pipeline error, got %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("verified = %d, want 2 (all degraded)", len(out))
	}
	if out[0].Reliability.Status != "unknown" {
		t.Errorf("degraded status = %s, want unknown", out[0].Reliability.Status)
	}
}

func TestVerifyCandidatesSkipsSearchWhenDisabled(t *testing.T) {
	se := &fakeSearcher{items: []websearch.SearchItem{{Title: "t", URL: "u"}}}
	f := newSerialFakeLLM([]any{map[string]any{"status": "unknown"}})
	v := New(f, &fakeStore{}, se, nil, nil, Config{UseWebSearch: false})
	if _, err := v.VerifyCandidates(context.Background(), "agent-a", cands(1)); err != nil {
		t.Fatal(err)
	}
	if se.calls != 0 {
		t.Errorf("searcher calls = %d, want 0", se.calls)
	}
}

func TestVerifyCandidatesSkipsSearchWhenSubjective(t *testing.T) {
	se := &fakeSearcher{items: []websearch.SearchItem{{Title: "t", URL: "u"}}}
	f := newSerialFakeLLM([]any{map[string]any{"status": "supported", "confidence": 0.8}})
	v := New(f, &fakeStore{}, se, nil, nil, Config{UseWebSearch: true, SearchMax: 5})
	c := cands(1)
	c[0].Subjective = true
	out, err := v.VerifyCandidates(context.Background(), "agent-a", c)
	if err != nil {
		t.Fatalf("VerifyCandidates error: %v", err)
	}
	if se.calls != 0 {
		t.Errorf("searcher calls = %d, want 0 (subjective skips web)", se.calls)
	}
	if len(out) != 1 {
		t.Fatalf("verified = %d, want 1", len(out))
	}
	if !out[0].Subjective {
		t.Error("subjective flag not propagated")
	}
	// The LLM verdict still runs for subjective candidates.
	if out[0].Reliability.Status != "supported" {
		t.Errorf("status = %s, want supported (verification still runs)", out[0].Reliability.Status)
	}
}

func TestVerifyCandidatesRelationUpdate(t *testing.T) {
	hist := &store.InterestPoint{ID: "old1", Name: "旧偏好", Summary: "喜欢 X"}
	f := newSerialFakeLLM([]any{map[string]any{
		"status": "supported", "confidence": 0.8,
		"relation": "update", "relation_reason": "用户改主意了",
	}})
	ri := &fakeRetriever{hits: []vec.Hit{{ID: "old1", Kind: "interest_point", Score: 0.92}}}
	emb := &fakeEmbedder{}
	v := New(f, &fakeStore{ip: hist}, nil, ri, emb, Config{})
	out, err := v.VerifyCandidates(context.Background(), "agent-a", cands(1))
	if err != nil {
		t.Fatalf("VerifyCandidates error: %v", err)
	}
	if emb.calls != 1 {
		t.Errorf("embedder calls = %d, want 1", emb.calls)
	}
	got := out[0]
	if got.Relation != RelationUpdate {
		t.Errorf("relation = %q, want update", got.Relation)
	}
	if got.RelationToID != "old1" {
		t.Errorf("relation_to_id = %q, want old1", got.RelationToID)
	}
	if got.RelationReason != "用户改主意了" {
		t.Errorf("relation_reason = %q", got.RelationReason)
	}
}

func TestVerifyCandidatesRelationSupersedeAndDelete(t *testing.T) {
	hist := &store.InterestPoint{ID: "old2", Name: "旧观点"}
	ri := &fakeRetriever{hits: []vec.Hit{{ID: "old2", Kind: "interest_point", Score: 0.9}}}
	// Concurrent candidate↔response mapping is not deterministic, so each
	// relation is verified with a single-candidate call.
	for _, tc := range []struct {
		wantRel string
		resp    map[string]any
	}{
		{"supersede", map[string]any{"relation": "supersede"}},
		{"delete", map[string]any{"relation": "delete"}},
	} {
		t.Run(tc.wantRel, func(t *testing.T) {
			f := newSerialFakeLLM([]any{tc.resp})
			v := New(f, &fakeStore{ip: hist}, nil, ri, &fakeEmbedder{}, Config{})
			out, err := v.VerifyCandidates(context.Background(), "agent-a", cands(1))
			if err != nil {
				t.Fatalf("VerifyCandidates error: %v", err)
			}
			if out[0].Relation != Relation(tc.wantRel) || out[0].RelationToID != "old2" {
				t.Errorf("relation = %q to %q, want %s/old2", out[0].Relation, out[0].RelationToID, tc.wantRel)
			}
		})
	}
}

func TestVerifyCandidatesRelationNoneWithoutHistory(t *testing.T) {
	f := newSerialFakeLLM([]any{map[string]any{"relation": "update"}})
	// No retriever/embedder → no history → relation forced to none.
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	out, err := v.VerifyCandidates(context.Background(), "agent-a", cands(1))
	if err != nil {
		t.Fatalf("VerifyCandidates error: %v", err)
	}
	if out[0].Relation != RelationNone || out[0].RelationToID != "" {
		t.Errorf("relation = %q to %q, want none to empty", out[0].Relation, out[0].RelationToID)
	}
}

func TestWebEvidenceLocators(t *testing.T) {
	se := &fakeSearcher{items: []websearch.SearchItem{{Title: "t", URL: "https://x.example", Snippet: "snip"}}}
	f := newSerialFakeLLM([]any{map[string]any{"status": "supported", "confidence": 0.8}})
	v := New(f, &fakeStore{}, se, nil, nil, Config{UseWebSearch: true, SearchMax: 5})
	out, err := v.VerifyCandidates(context.Background(), "agent-a", cands(1))
	if err != nil {
		t.Fatalf("VerifyCandidates error: %v", err)
	}
	ev := out[0].Reliability.Evidence
	if len(ev) == 0 {
		t.Fatal("no evidence")
	}
	if ev[0].URL != "https://x.example" {
		t.Errorf("evidence URL = %q", ev[0].URL)
	}
	if ev[0].Query == "" {
		t.Error("evidence Query empty")
	}
	if ev[0].CapturedAt.IsZero() {
		t.Error("evidence CapturedAt zero")
	}
}

func TestSessionEvidenceHasTurnRange(t *testing.T) {
	// Subjective candidate → no web evidence → session evidence with TurnRange.
	f := newSerialFakeLLM([]any{map[string]any{"status": "supported"}})
	v := New(f, &fakeStore{}, nil, nil, nil, Config{UseWebSearch: true})
	c := cands(1)
	c[0].Subjective = true
	c[0].TurnRange = [2]int{3, 4}
	out, err := v.VerifyCandidates(context.Background(), "agent-a", c)
	if err != nil {
		t.Fatalf("VerifyCandidates error: %v", err)
	}
	ev := out[0].Reliability.Evidence
	if len(ev) != 1 || ev[0].Kind != "session" {
		t.Fatalf("evidence = %+v, want single session evidence", ev)
	}
	if ev[0].TurnRange != [2]int{3, 4} {
		t.Errorf("turn_range = %v, want [3 4]", ev[0].TurnRange)
	}
}

func TestCheckClaims(t *testing.T) {
	f := newSerialFakeLLM([]any{map[string]any{
		"claims": []map[string]any{
			{"text": "A is true", "confidence": 0.9, "status": "supported", "evidence": []string{"reason"}},
			{"text": "B is true", "confidence": 0.6, "status": "contested"},
		},
	}})
	pt := store.InterestPoint{ID: "ip1", AgentID: "a", Name: "X", Summary: "S",
		Reliability: store.Reliability{Confidence: 0.9, Status: "supported"},
		Freshness:   store.Freshness{Level: "fresh"}}
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	claims, err := v.CheckClaims(context.Background(), "a", []store.InterestPoint{pt})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %d, want 2", len(claims))
	}
	if claims[0].Status != "supported" || claims[1].Status != "contested" {
		t.Errorf("claim statuses = %s/%s", claims[0].Status, claims[1].Status)
	}
	if claims[0].PageID != "ip1" {
		t.Errorf("page_id = %s, want ip1", claims[0].PageID)
	}
}

func TestCheckClaimsDegradesOnError(t *testing.T) {
	f := newSerialFakeLLM(nil)
	f.err = context.Canceled
	pt := store.InterestPoint{ID: "ip1", AgentID: "a", Name: "X", Summary: "S",
		Reliability: store.Reliability{Confidence: 0.8, Status: "supported"},
		Freshness:   store.Freshness{Level: "fresh"}}
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	claims, err := v.CheckClaims(context.Background(), "a", []store.InterestPoint{pt})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Text != "S" {
		t.Fatalf("degraded claims = %+v", claims)
	}
}

func TestFlagContradictions(t *testing.T) {
	c1 := store.Claim{ID: "c1", Text: "MySQL is the best"}
	c2 := store.Claim{ID: "c2", Text: "SQLite is the best"}
	f := newSerialFakeLLM([]any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "MySQL is the best", "right_text": "SQLite is the best", "description": "two DBs both called best"},
		},
	}})
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 1 {
		t.Fatalf("contradictions = %d, want 1", len(cons))
	}
	if cons[0].LeftID != "c1" || cons[0].RightID != "c2" || cons[0].Status != "open" {
		t.Errorf("contradiction = %+v", cons[0])
	}
}

func TestFlagContradictionsDegradesOnError(t *testing.T) {
	f := newSerialFakeLLM(nil)
	f.err = context.DeadlineExceeded
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{{ID: "c1", Text: "x"}, {ID: "c2", Text: "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 0 {
		t.Errorf("contradictions = %d, want 0 (degraded)", len(cons))
	}
}

func TestFlagContradictionsFalse(t *testing.T) {
	c1 := store.Claim{ID: "c1", Text: "MySQL is the best"}
	c2 := store.Claim{ID: "c2", Text: "SQLite is the best"}
	f := newSerialFakeLLM([]any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "MySQL is the best", "right_text": "SQLite is the best", "description": "both claim best", "is_contradiction": false, "confidence": 0.9},
		},
	}})
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 0 {
		t.Fatalf("contradictions = %d, want 0 (is_contradiction=false)", len(cons))
	}
}

func TestFlagContradictionsMissingField(t *testing.T) {
	// Legacy model omitting is_contradiction must still be accepted (defaults
	// to true) instead of silently zeroing recall.
	c1 := store.Claim{ID: "c1", Text: "A"}
	c2 := store.Claim{ID: "c2", Text: "B"}
	f := newSerialFakeLLM([]any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "A", "right_text": "B", "description": "conflict"},
		},
	}})
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 1 {
		t.Fatalf("contradictions = %d, want 1 (missing field defaults to true)", len(cons))
	}
}

func TestFlagContradictionsNegativeDesc(t *testing.T) {
	// LLM marks true but describes the pair as a denial → B overrides to skip.
	c1 := store.Claim{ID: "c1", Text: "A"}
	c2 := store.Claim{ID: "c2", Text: "B"}
	f := newSerialFakeLLM([]any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "A", "right_text": "B", "description": "not a contradiction, they are consistent", "is_contradiction": true, "confidence": 0.9},
		},
	}})
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 0 {
		t.Fatalf("contradictions = %d, want 0 (negative description)", len(cons))
	}
}

func TestFlagContradictionsNoFalseDrop(t *testing.T) {
	// Negation forms of the weak words (不一致 / inconsistent) describe real
	// conflict and must not be dropped by the denial filter.
	for _, tc := range []struct {
		name     string
		desc     string
		wantCons int
	}{
		{"chinese_inconsistent", "这两个声明不一致", 1},
		{"english_inconsistent", "the claims are inconsistent", 1},
		{"denial", "they are not contradictory", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSerialFakeLLM([]any{map[string]any{
				"contradictions": []map[string]any{
					{"left_text": "A", "right_text": "B", "description": tc.desc, "is_contradiction": true, "confidence": 0.9},
				},
			}})
			v := New(f, &fakeStore{}, nil, nil, nil, Config{})
			cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{{ID: "c1", Text: "A"}, {ID: "c2", Text: "B"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(cons) != tc.wantCons {
				t.Fatalf("contradictions = %d, want %d", len(cons), tc.wantCons)
			}
		})
	}
}

func TestFlagContradictionsConfidenceThreshold(t *testing.T) {
	c1 := store.Claim{ID: "c1", Text: "A"}
	c2 := store.Claim{ID: "c2", Text: "B"}
	resp := func() map[string]any {
		return map[string]any{
			"contradictions": []map[string]any{
				{"left_text": "A", "right_text": "B", "description": "conflict", "is_contradiction": true, "confidence": 0.4},
			},
		}
	}
	// Configured threshold: below it is dropped.
	f := newSerialFakeLLM([]any{resp()})
	v := New(f, &fakeStore{}, nil, nil, nil, Config{MinConfidence: 0.6})
	cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 0 {
		t.Fatalf("contradictions = %d, want 0 (below threshold)", len(cons))
	}
	// Default (no threshold): accepted.
	f2 := newSerialFakeLLM([]any{resp()})
	v2 := New(f2, &fakeStore{}, nil, nil, nil, Config{})
	cons2, err := v2.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons2) != 1 {
		t.Fatalf("contradictions = %d, want 1 (no threshold configured)", len(cons2))
	}
}

func TestFlagContradictionsCanonicalID(t *testing.T) {
	c1 := store.Claim{ID: "aaa", Text: "A"}
	c2 := store.Claim{ID: "zzz", Text: "B"}
	fwd := map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "A", "right_text": "B", "description": "conflict", "is_contradiction": true},
		},
	}
	rev := map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "B", "right_text": "A", "description": "conflict", "is_contradiction": true},
		},
	}
	f := newSerialFakeLLM([]any{fwd, rev})
	v := New(f, &fakeStore{}, nil, nil, nil, Config{})
	cons1, err := v.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	cons2, err := v.FlagContradictions(context.Background(), "a", []store.Claim{c1, c2})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons1) != 1 || len(cons2) != 1 {
		t.Fatalf("contradictions = %d/%d, want 1/1", len(cons1), len(cons2))
	}
	if cons1[0].ID != cons2[0].ID {
		t.Errorf("reversed pair ids differ: %q vs %q", cons1[0].ID, cons2[0].ID)
	}
	if cons1[0].LeftID != "aaa" || cons1[0].RightID != "zzz" {
		t.Errorf("forward left/right not preserved: %+v", cons1[0])
	}
	if cons2[0].LeftID != "zzz" || cons2[0].RightID != "aaa" {
		t.Errorf("reversed left/right not preserved: %+v", cons2[0])
	}
}

func TestBuildContradictionPromptLanguage(t *testing.T) {
	zh := (&service{cfg: Config{Language: "中文"}}).buildContradictionPrompt([]store.Claim{{ID: "c1", Text: "x"}})
	if !strings.Contains(zh, "in '中文'") {
		t.Errorf("Chinese-language prompt missing language directive:\n%s", zh)
	}
	if !strings.Contains(zh, "is_contradiction") {
		t.Errorf("prompt missing is_contradiction field:\n%s", zh)
	}
	en := (&service{cfg: Config{Language: "English"}}).buildContradictionPrompt([]store.Claim{{ID: "c1", Text: "x"}})
	if !strings.Contains(en, "in 'English'") {
		t.Errorf("English-language prompt missing language directive:\n%s", en)
	}
	if !strings.Contains(en, "contradict") {
		t.Errorf("English prompt missing contradict:\n%s", en)
	}
}

func TestFindClaimUniquePrefix(t *testing.T) {
	claims := []store.Claim{
		{ID: "a", Text: "PostgreSQL is the best database"},
		{ID: "b", Text: "PostgreSQL is the best open-source database"},
	}
	if got := findClaim(claims, "PostgreSQL is the best"); got != nil {
		t.Errorf("ambiguous prefix matched %q, want nil", got.ID)
	}
	if got := findClaim(claims, "PostgreSQL is the best database"); got == nil || got.ID != "a" {
		t.Errorf("exact match failed: %+v", got)
	}
	claims2 := []store.Claim{{ID: "x", Text: "Go is compiled"}, {ID: "y", Text: "Rust is safe"}}
	if got := findClaim(claims2, "Go is comp"); got == nil || got.ID != "x" {
		t.Errorf("single prefix match failed: %+v", got)
	}
}

func TestGradeForRecallInterestPoint(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip1", AgentID: "a", Name: "Alpha",
		Reliability: store.Reliability{Confidence: 0.7, Status: "supported"},
		Freshness:   store.Freshness{Level: "aging"}}
	st := &fakeStore{ip: ip}
	v := New(newSerialFakeLLM(nil), st, nil, nil, nil, Config{})
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{
		{ID: "ip1", AgentID: "a", Kind: "interest_point", Score: 0.8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("graded = %d, want 1", len(got))
	}
	g := got[0]
	if g.Title != "Alpha" || g.Confidence != 0.7 || g.Status != "supported" || g.FreshLevel != "aging" {
		t.Errorf("graded = %+v", g)
	}
	if g.Note == "" {
		t.Error("expected self-check note")
	}
}

func TestGradeForRecallArchivedFiltered(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip1", AgentID: "a", Name: "Alpha", Status: "archived",
		Reliability: store.Reliability{Confidence: 0.7, Status: "supported"},
		Freshness:   store.Freshness{Level: "aging"}}
	st := &fakeStore{ip: ip}
	v := New(newSerialFakeLLM(nil), st, nil, nil, nil, Config{})
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{
		{ID: "ip1", AgentID: "a", Kind: "interest_point", Score: 0.8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("graded = %d, want 0 (archived filtered)", len(got))
	}
}

func TestGradeForRecallSupersededPageFiltered(t *testing.T) {
	st := &fakeStore{page: &store.Page{ID: "pg1", Title: "Page", Status: "superseded", Claims: []store.Claim{
		{Text: "a", Confidence: 0.9, Status: "supported"},
	}}}
	v := New(newSerialFakeLLM(nil), st, nil, nil, nil, Config{})
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "pg1", Kind: "wiki_page", Score: 0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("graded = %d, want 0 (superseded page filtered)", len(got))
	}
}

// fakeSubStore extends fakeStore with a configurable replacement so grading
// can exercise silent substitution of superseded entities.
type fakeSubStore struct {
	ip       *store.InterestPoint
	page     *store.Page
	replPage *store.Page
	replIP   *store.InterestPoint
}

func (f *fakeSubStore) GetInterestPoint(_ context.Context, _, id string) (*store.InterestPoint, error) {
	if id == f.replIP.ID {
		return f.replIP, nil
	}
	return f.ip, nil
}
func (f *fakeSubStore) UpsertInterestPoint(_ context.Context, p store.InterestPoint) error {
	f.ip = &p
	return nil
}
func (f *fakeSubStore) GetPage(_ context.Context, _, id string) (*store.Page, error) {
	if f.replPage != nil && id == f.replPage.ID {
		return f.replPage, nil
	}
	return f.page, nil
}
func (f *fakeSubStore) ListClaims(_ context.Context, _, id string) ([]store.Claim, error) {
	pg := f.page
	if f.replPage != nil && id == f.replPage.ID {
		pg = f.replPage
	}
	if pg == nil {
		return nil, nil
	}
	return pg.Claims, nil
}
func (f *fakeSubStore) ResolveReplacement(context.Context, string, string) (*store.Replacement, error) {
	if f.replPage != nil {
		return &store.Replacement{InterestPointID: f.replIP.ID, Page: f.replPage}, nil
	}
	if f.replIP != nil {
		return &store.Replacement{InterestPointID: f.replIP.ID}, nil
	}
	return nil, nil
}

func TestGradeForRecallSubstitutesSupersededPage(t *testing.T) {
	st := &fakeSubStore{
		page: &store.Page{ID: "pg-old", Title: "旧页", Status: "superseded", Claims: []store.Claim{
			{Text: "old", Confidence: 0.9, Status: "supported"},
		}},
		replPage: &store.Page{ID: "pg-new", Title: "新页", Status: "active", Claims: []store.Claim{
			{Text: "new", Confidence: 0.95, Status: "supported", Freshness: store.Freshness{Level: "fresh"}},
		}},
		replIP: &store.InterestPoint{ID: "ip-new", Name: "新点"},
	}
	v := New(newSerialFakeLLM(nil), st, nil, nil, nil, Config{})
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "pg-old", Kind: "wiki_page", Score: 0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("graded = %d, want 1 (substituted)", len(got))
	}
	g := got[0]
	if g.Hit.ID != "pg-new" || g.Hit.Kind != "wiki_page" {
		t.Errorf("substituted hit = %+v, want pg-new", g.Hit)
	}
	if g.Title != "新页" {
		t.Errorf("title = %q, want 新页", g.Title)
	}
}

func TestGradeForRecallSubstitutesArchivedInterestPoint(t *testing.T) {
	st := &fakeSubStore{
		ip:     &store.InterestPoint{ID: "ip-old", Name: "旧点", Status: "archived"},
		replIP: &store.InterestPoint{ID: "ip-new", Name: "新点"},
		replPage: &store.Page{ID: "pg-new", Title: "新页", Status: "active", Claims: []store.Claim{
			{Text: "n", Confidence: 0.9, Status: "supported"},
		}},
	}
	v := New(newSerialFakeLLM(nil), st, nil, nil, nil, Config{})
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "ip-old", Kind: "interest_point", Score: 0.8}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hit.ID != "pg-new" {
		t.Errorf("graded = %+v, want substituted pg-new", got)
	}
}

func TestGradeForRecallPage(t *testing.T) {
	st := &fakeStore{page: &store.Page{ID: "pg1", Title: "Page", Claims: []store.Claim{
		{Text: "a", Confidence: 0.4, Status: "contested", Freshness: store.Freshness{Level: "stale"}},
		{Text: "b", Confidence: 0.9, Status: "supported", Freshness: store.Freshness{Level: "fresh"}},
	}}}
	v := New(newSerialFakeLLM(nil), st, nil, nil, nil, Config{})
	got, err := v.GradeForRecall(context.Background(), "a", []vec.Hit{{ID: "pg1", Kind: "wiki_page", Score: 0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Title != "Page" || got[0].Status != "supported" || got[0].FreshLevel != "fresh" {
		t.Errorf("page graded = %+v", got[0])
	}
}

func TestFeedbackWriteBumpsInterestPoint(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip1", AgentID: "a", Name: "Alpha", SeenCount: 3, Importance: 0.5}
	st := &fakeStore{ip: ip}
	v := New(newSerialFakeLLM(nil), st, nil, nil, nil, Config{})
	err := v.FeedbackWrite(context.Background(), "a", []vec.Hit{
		{ID: "ip1", Kind: "interest_point"},
		{ID: "pg1", Kind: "wiki_page"}, // ignored
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.ip.SeenCount != 4 {
		t.Errorf("seen_count = %d, want 4", st.ip.SeenCount)
	}
	if st.ip.Importance != 0.55 {
		t.Errorf("importance = %f, want 0.55", st.ip.Importance)
	}
}

func TestVerifyCandidatesParallel(t *testing.T) {
	// Many candidates, serial fake LLM (single-slot mutex) — proves
	// VerifyCandidates converges with correct count and per-candidate status.
	n := 12
	resps := make([]any, n)
	for i := range resps {
		resps[i] = map[string]any{"status": "supported", "confidence": 0.8}
	}
	f := newSerialFakeLLM(resps)
	v := New(f, &fakeStore{}, nil, nil, nil, Config{MaxConcurrency: 4})
	out, err := v.VerifyCandidates(context.Background(), "agent-a", cands(n))
	if err != nil {
		t.Fatalf("VerifyCandidates error: %v", err)
	}
	if len(out) != n {
		t.Fatalf("verified = %d, want %d", len(out), n)
	}
	for i, o := range out {
		if o.Reliability.Status != "supported" {
			t.Errorf("candidate %d status = %s, want supported", i, o.Reliability.Status)
		}
	}
}

// topicEmbedder returns 2-D one-hot vectors: claims containing "topicA" get
// [1,0], anything else [0,1]. Pairs within a topic have cosine 1, cross-topic
// pairs 0 — a deterministic stand-in for real embeddings.
func topicEmbedder() *fakeEmbedder {
	return &fakeEmbedder{batchFn: func(texts []string) [][]float32 {
		out := make([][]float32, len(texts))
		for i, t := range texts {
			if strings.Contains(t, "topicA") {
				out[i] = []float32{1, 0}
			} else {
				out[i] = []float32{0, 1}
			}
		}
		return out
	}}
}

func TestFlagContradictionsSemanticGrouping(t *testing.T) {
	claims := []store.Claim{
		{ID: "c0", Text: "topicA claim one"},
		{ID: "c1", Text: "topicA claim two"},
		{ID: "c2", Text: "topicA claim three"},
		{ID: "c3", Text: "topicB claim one"},
		{ID: "c4", Text: "topicB claim two"},
	}
	f := newSerialFakeLLM([]any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "topicA claim one", "right_text": "topicA claim two", "description": "conflict", "is_contradiction": true, "confidence": 0.9},
		},
	}})
	v := New(f, &fakeStore{}, nil, nil, topicEmbedder(), Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", claims)
	if err != nil {
		t.Fatal(err)
	}
	if f.callCount() != 1 {
		t.Fatalf("llm calls = %d, want 1", f.callCount())
	}
	// The prompt must list only same-topic candidate pairs with full texts.
	p := f.lastPrompt()
	if !strings.Contains(p, `"topicA claim one" ↔ "topicA claim two"`) {
		t.Errorf("prompt missing same-topic candidate pair:\n%s", p)
	}
	if strings.Contains(p, `"topicB claim one"`) && strings.Contains(p, `"topicA claim one" ↔ "topicB claim one"`) {
		t.Errorf("prompt contains cross-topic candidate pair:\n%s", p)
	}
	if len(cons) != 1 {
		t.Fatalf("contradictions = %d, want 1", len(cons))
	}
	if cons[0].LeftID != "c0" || cons[0].RightID != "c1" {
		t.Errorf("contradiction = %+v", cons[0])
	}
}

func TestFlagContradictionsSemanticGroupingRejectsOutOfSet(t *testing.T) {
	claims := []store.Claim{
		{ID: "c0", Text: "topicA claim one"},
		{ID: "c1", Text: "topicA claim two"},
		{ID: "c3", Text: "topicB claim one"},
	}
	// The LLM returns a pair that never passed the embedding pre-filter.
	f := newSerialFakeLLM([]any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "topicA claim one", "right_text": "topicB claim one", "description": "conflict", "is_contradiction": true, "confidence": 0.9},
		},
	}})
	v := New(f, &fakeStore{}, nil, nil, topicEmbedder(), Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", claims)
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 0 {
		t.Fatalf("contradictions = %d, want 0 (pair outside candidate set)", len(cons))
	}
}

func TestFlagContradictionsSemanticGroupingEmpty(t *testing.T) {
	// Every claim is its own orthogonal topic → no candidate pairs → the LLM
	// is never called (vs the full scan which always calls once per window).
	claims := []store.Claim{
		{ID: "c0", Text: "topicA claim one"},
		{ID: "c1", Text: "topicB claim one"},
		{ID: "c2", Text: "topicC claim one"},
		{ID: "c3", Text: "topicD claim one"},
	}
	emb := &fakeEmbedder{batchFn: func(texts []string) [][]float32 {
		out := make([][]float32, len(texts))
		for i := range texts {
			vec := make([]float32, 4)
			vec[i] = 1
			out[i] = vec
		}
		return out
	}}
	f := newSerialFakeLLM(nil)
	v := New(f, &fakeStore{}, nil, nil, emb, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", claims)
	if err != nil {
		t.Fatal(err)
	}
	if f.callCount() != 0 {
		t.Fatalf("llm calls = %d, want 0 (no candidates)", f.callCount())
	}
	if len(cons) != 0 {
		t.Fatalf("contradictions = %d, want 0", len(cons))
	}
}

func TestFlagContradictionsSemanticGroupingDegrades(t *testing.T) {
	// EmbedBatch error → fall back to the full-scan prompt; contradictions
	// are still detected.
	emb := &fakeEmbedder{err: context.DeadlineExceeded}
	claims := []store.Claim{{ID: "c0", Text: "A is best"}, {ID: "c1", Text: "B is best"}}
	f := newSerialFakeLLM([]any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "A is best", "right_text": "B is best", "description": "conflict", "is_contradiction": true, "confidence": 0.9},
		},
	}})
	v := New(f, &fakeStore{}, nil, nil, emb, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", claims)
	if err != nil {
		t.Fatal(err)
	}
	if f.callCount() != 1 {
		t.Fatalf("llm calls = %d, want 1 (degraded to full scan)", f.callCount())
	}
	p := f.lastPrompt()
	if !strings.Contains(p, "Below are claims") && !strings.Contains(p, "下面是记忆 wiki") {
		t.Errorf("degraded prompt does not look like the full scan:\n%s", p)
	}
	if len(cons) != 1 {
		t.Fatalf("contradictions = %d, want 1", len(cons))
	}
}

func TestSemanticCandidatesThresholdAndCap(t *testing.T) {
	group := []store.Claim{
		{ID: "a1", Text: "topicA claim one"},
		{ID: "a2", Text: "topicA claim two"},
		{ID: "a3", Text: "topicA claim three"},
		{ID: "a4", Text: "topicA claim four"},
		{ID: "a5", Text: "topicA claim five"},
		{ID: "b1", Text: "topicB claim one"},
		{ID: "b2", Text: "topicB claim two"},
		{ID: "b3", Text: "topicB claim three"},
	}
	s := &service{embed: topicEmbedder(), cfg: Config{SimThreshold: 0.45, MaxCandidates: 5}}
	cands, err := s.semanticCandidates(context.Background(), group)
	if err != nil {
		t.Fatal(err)
	}
	// A-internal C(5,2)=10 + B-internal C(3,2)=3 = 13 pairs, capped at 5.
	if len(cands) != 5 {
		t.Fatalf("candidates = %d, want 5 (capped)", len(cands))
	}
	// All retained pairs are A-internal (indices 0..4) and sorted desc.
	for _, c := range cands {
		if c.i >= 5 || c.j >= 5 {
			t.Errorf("cross-topic pair retained: %+v", c)
		}
		if c.sim != 1 {
			t.Errorf("pair sim = %f, want 1", c.sim)
		}
	}
}

func TestBuildCandidatePromptLanguage(t *testing.T) {
	group := []store.Claim{{ID: "c0", Text: "A is best"}, {ID: "c1", Text: "B is best"}}
	cands := []candidatePair{{i: 0, j: 1, sim: 0.9}}
	zh := (&service{cfg: Config{Language: "中文"}}).buildCandidatePrompt(group, cands)
	if !strings.Contains(zh, "in '中文'") {
		t.Errorf("Chinese-language prompt missing language directive:\n%s", zh)
	}
	if !strings.Contains(zh, `"A is best" ↔ "B is best"`) {
		t.Errorf("prompt missing candidate pair text:\n%s", zh)
	}
	en := (&service{cfg: Config{Language: "English"}}).buildCandidatePrompt(group, cands)
	if !strings.Contains(en, "pre-filtered candidate pairs") {
		t.Errorf("English prompt missing candidate section:\n%s", en)
	}
}

var _ = time.Now
