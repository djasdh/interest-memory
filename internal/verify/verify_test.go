package verify

import (
	"context"
	"encoding/json"
	"testing"

	"interest-memory/internal/fork"
	"interest-memory/internal/llm"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
)

// fakeLLM returns a canned JSON per call, optionally erroring.
type fakeLLM struct {
	responses []any // one per ChatJSON call, consumed in order
	idx       int
	err       error
	lastMsgs  []llm.Message
}

func (f *fakeLLM) ChatJSON(_ context.Context, msgs []llm.Message, out any) error {
	f.lastMsgs = msgs
	if f.err != nil {
		return f.err
	}
	var payload []byte
	if f.idx < len(f.responses) {
		payload, _ = json.Marshal(f.responses[f.idx])
		f.idx++
	} else {
		payload = []byte("{}")
	}
	return json.Unmarshal(payload, out)
}

type fakeSearcher struct {
	items []SearchItem
	err   error
	calls int
}

func (f *fakeSearcher) Search(_ context.Context, _ string, _ int) ([]SearchItem, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
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

func cands(n int) []fork.Candidate {
	out := make([]fork.Candidate, n)
	for i := range out {
		out[i] = fork.Candidate{Topic: "topic", Reason: "reason", Confidence: 0.9}
	}
	return out
}

func TestVerifyCandidatesSupported(t *testing.T) {
	f := &fakeLLM{responses: []any{map[string]any{
		"supported":       true,
		"confidence":       0.9,
		"status":           "supported",
		"evidence":         []string{"matches known facts"},
		"freshness_level":  "fresh",
		"ttl_days":         90,
	}}}
	v := New(f, &fakeStore{}, &fakeSearcher{items: []SearchItem{{Title: "t", URL: "u", Snippet: "s"}}}, Config{UseWebSearch: true, SearchMax: 5})
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
	f := &fakeLLM{err: context.DeadlineExceeded}
	v := New(f, &fakeStore{}, nil, Config{UseWebSearch: false})
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
	se := &fakeSearcher{items: []SearchItem{{Title: "t", URL: "u"}}}
	f := &fakeLLM{responses: []any{map[string]any{"status": "unknown"}}}
	v := New(f, &fakeStore{}, se, Config{UseWebSearch: false})
	if _, err := v.VerifyCandidates(context.Background(), "agent-a", cands(1)); err != nil {
		t.Fatal(err)
	}
	if se.calls != 0 {
		t.Errorf("searcher calls = %d, want 0", se.calls)
	}
}

func TestCheckClaims(t *testing.T) {
	f := &fakeLLM{responses: []any{map[string]any{
		"claims": []map[string]any{
			{"text": "A is true", "confidence": 0.9, "status": "supported", "evidence": []string{"reason"}},
			{"text": "B is true", "confidence": 0.6, "status": "contested"},
		},
	}}}
	pt := store.InterestPoint{ID: "ip1", AgentID: "a", Name: "X", Summary: "S",
		Reliability: store.Reliability{Confidence: 0.9, Status: "supported"},
		Freshness:   store.Freshness{Level: "fresh"}}
	v := New(f, &fakeStore{}, nil, Config{})
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
	f := &fakeLLM{err: context.Canceled}
	pt := store.InterestPoint{ID: "ip1", AgentID: "a", Name: "X", Summary: "S",
		Reliability: store.Reliability{Confidence: 0.8, Status: "supported"},
		Freshness:   store.Freshness{Level: "fresh"}}
	v := New(f, &fakeStore{}, nil, Config{})
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
	f := &fakeLLM{responses: []any{map[string]any{
		"contradictions": []map[string]any{
			{"left_text": "MySQL is the best", "right_text": "SQLite is the best", "description": "two DBs both called best"},
		},
	}}}
	v := New(f, &fakeStore{}, nil, Config{})
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
	f := &fakeLLM{err: context.DeadlineExceeded}
	v := New(f, &fakeStore{}, nil, Config{})
	cons, err := v.FlagContradictions(context.Background(), "a", []store.Claim{{ID: "c1", Text: "x"}, {ID: "c2", Text: "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 0 {
		t.Errorf("contradictions = %d, want 0 (degraded)", len(cons))
	}
}

func TestGradeForRecallInterestPoint(t *testing.T) {
	ip := &store.InterestPoint{ID: "ip1", AgentID: "a", Name: "Alpha",
		Reliability: store.Reliability{Confidence: 0.7, Status: "supported"},
		Freshness:   store.Freshness{Level: "aging"}}
	st := &fakeStore{ip: ip}
	v := New(&fakeLLM{}, st, nil, Config{})
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

func TestGradeForRecallPage(t *testing.T) {
	st := &fakeStore{page: &store.Page{ID: "pg1", Title: "Page", Claims: []store.Claim{
		{Text: "a", Confidence: 0.4, Status: "contested", Freshness: store.Freshness{Level: "stale"}},
		{Text: "b", Confidence: 0.9, Status: "supported", Freshness: store.Freshness{Level: "fresh"}},
	}}}
	v := New(&fakeLLM{}, st, nil, Config{})
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
	v := New(&fakeLLM{}, st, nil, Config{})
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
