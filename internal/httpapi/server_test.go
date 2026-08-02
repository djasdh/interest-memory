package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"interest-memory/internal/recall"
	"interest-memory/internal/store"
	"interest-memory/internal/worker"
)

// fakeService implements Service for handler tests.
type fakeService struct {
	saveErr   error
	recallOut string
	pts       []store.InterestPoint
	pages     []store.Page
	stats     map[string]int
	forkTx    *store.Transcript
	searchRes []recall.Result
	byIDRes   *recall.Result
	logs      []store.ChangeLog
	savedTx   *store.Transcript
}

func (f *fakeService) ProcessSession(context.Context, string, store.Transcript) error { return nil }
func (f *fakeService) SaveTranscript(_ context.Context, t store.Transcript) error {
	f.savedTx = &t
	return f.saveErr
}
func (f *fakeService) Recall(_ context.Context, _, _ string) (string, error)          { return f.recallOut, nil }
func (f *fakeService) ListInterestPoints(context.Context, string) ([]store.InterestPoint, error) {
	return f.pts, nil
}
func (f *fakeService) ListPages(context.Context, string, store.PageType) ([]store.Page, error) {
	return f.pages, nil
}
func (f *fakeService) Stats(context.Context, string) (map[string]int, error) { return f.stats, nil }
func (f *fakeService) ForkManual(context.Context, string) (*store.Transcript, error) {
	return f.forkTx, nil
}
func (f *fakeService) Search(_ context.Context, _, _ string, _ int) ([]recall.Result, error) {
	return f.searchRes, nil
}
func (f *fakeService) GetByID(_ context.Context, _, _ string) (*recall.Result, error) {
	return f.byIDRes, nil
}
func (f *fakeService) ListLogs(_ context.Context, _ string, _, _ int) ([]store.ChangeLog, error) {
	return f.logs, nil
}

// fakeWorker implements Worker for handler tests.
type fakeWorker struct {
	jobID string
	job   *worker.Job
	err   error
}

func (f *fakeWorker) Enqueue(context.Context, string, string) (string, error) { return f.jobID, f.err }
func (f *fakeWorker) GetJob(context.Context, string) (*worker.Job, error)     { return f.job, f.err }

func newTestServer(fs Service, fw Worker) *httptest.Server {
	return httptest.NewServer(NewServer(fs, fw).Handler())
}

func TestSessionsEnqueue(t *testing.T) {
	fs := &fakeService{}
	fw := &fakeWorker{jobID: "job-1"}
	ts := newTestServer(fs, fw)
	defer ts.Close()

	body := `{"session_id":"s1","turn_count":3,"raw_turns":"[{\"role\":\"user\",\"content\":\"hi\"}]"}`
	resp, err := http.Post(ts.URL+"/api/v1/agent-a/sessions", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	if out["job_id"] != "job-1" {
		t.Errorf("job_id = %q", out["job_id"])
	}
}

func TestSessionsParsesSessionDate(t *testing.T) {
	fs := &fakeService{}
	fw := &fakeWorker{jobID: "job-1"}
	ts := newTestServer(fs, fw)
	defer ts.Close()

	body := `{"session_id":"s1","turn_count":2,"raw_turns":"[]","session_date":"2026-08-01T10:00:00Z"}`
	resp, err := http.Post(ts.URL+"/api/v1/agent-a/sessions", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if fs.savedTx == nil || fs.savedTx.SessionDate == nil {
		t.Fatalf("session_date not parsed: %+v", fs.savedTx)
	}
	want := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	if !fs.savedTx.SessionDate.Equal(want) {
		t.Errorf("session_date = %v, want %v", fs.savedTx.SessionDate, want)
	}
}

func TestSessionsIgnoresInvalidSessionDate(t *testing.T) {
	fs := &fakeService{}
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()

	body := `{"session_id":"s1","turn_count":2,"raw_turns":"[]","session_date":"not-a-date"}`
	resp, err := http.Post(ts.URL+"/api/v1/agent-a/sessions", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if fs.savedTx == nil || fs.savedTx.SessionDate != nil {
		t.Errorf("invalid session_date should be ignored: %+v", fs.savedTx)
	}
}

func TestSessionsRejectsEmptyRawTurns(t *testing.T) {
	ts := newTestServer(&fakeService{}, &fakeWorker{})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/agent-a/sessions", "application/json", strings.NewReader(`{"session_id":"s1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRecall(t *testing.T) {
	fs := &fakeService{recallOut: "- Go concurrency [interest_point]"}
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/agent-a/recall?query=go")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	if !strings.Contains(out["memory_context"], "Go concurrency") {
		t.Errorf("memory_context = %q", out["memory_context"])
	}
}

func TestInterestPointsAndWikiPages(t *testing.T) {
	fs := &fakeService{
		pts:   []store.InterestPoint{{ID: "ip1", AgentID: "agent-a", Name: "Go"}},
		pages: []store.Page{{ID: "pg1", AgentID: "agent-a", Title: "Wiki"}},
	}
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/agent-a/interest-points")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("interest-points status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/api/v1/agent-a/wiki/pages?type=concept")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wiki pages status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFork(t *testing.T) {
	fs := &fakeService{forkTx: &store.Transcript{SessionID: "s-old"}}
	fw := &fakeWorker{jobID: "job-f"}
	ts := newTestServer(fs, fw)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/agent-a/fork", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}

func TestForkNoTranscript(t *testing.T) {
	fs := &fakeService{} // forkTx nil → 404
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/agent-a/fork", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestJobGetWithAgentNamespace(t *testing.T) {
	done := timeNow()
	fw := &fakeWorker{job: &worker.Job{ID: "job-1", AgentID: "agent-a", SessionID: "s1", Status: worker.StatusDone, DoneAt: &done}}
	ts := newTestServer(&fakeService{}, fw)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/agent-a/jobs/job-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Wrong agent → 404.
	resp2, err2 := http.Get(ts.URL + "/api/v1/agent-b/jobs/job-1")
	if err2 != nil {
		t.Fatal(err2)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong-agent status = %d, want 404", resp2.StatusCode)
	}
}

func TestStats(t *testing.T) {
	fs := &fakeService{stats: map[string]int{"interest_points": 3}}
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/agent-a/stats")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSearchByQuery(t *testing.T) {
	fs := &fakeService{searchRes: []recall.Result{{
		Kind: "wiki_page", ID: "postgresql-page", Title: "PostgreSQL",
		Outlinks: []recall.EdgeRef{{ID: "related", Title: "相关页", Kind: store.EdgeRelated, Weight: 0.9}},
	}}}
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/agent-a/search?query=PostgreSQL")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", resp.StatusCode)
	}
	var body struct {
		Items []recall.Result `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "postgresql-page" {
		t.Errorf("items = %+v", body.Items)
	}
	if len(body.Items[0].Outlinks) != 1 || body.Items[0].Outlinks[0].Title != "相关页" {
		t.Errorf("outlinks = %+v", body.Items[0].Outlinks)
	}
}

func TestSearchById(t *testing.T) {
	fs := &fakeService{byIDRes: &recall.Result{Kind: "interest_point", ID: "ip-1", Title: "点"}}
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/agent-a/search?id=ip-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search by id status = %d", resp.StatusCode)
	}
	var body struct {
		Items []recall.Result `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "ip-1" {
		t.Errorf("items = %+v", body.Items)
	}
}

func TestSearchRequiresQueryOrId(t *testing.T) {
	ts := newTestServer(&fakeService{}, &fakeWorker{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/agent-a/search")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLogsEndpoint(t *testing.T) {
	fs := &fakeService{logs: []store.ChangeLog{
		{ID: "l1", AgentID: "agent-a", Action: "create", Title: "P1"},
	}}
	ts := newTestServer(fs, &fakeWorker{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/agent-a/logs?limit=10&offset=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs status = %d", resp.StatusCode)
	}
	var body struct {
		Items []store.ChangeLog `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "l1" || body.Items[0].Title != "P1" {
		t.Errorf("items = %+v", body.Items)
	}
}

func TestRoutesIncludesNoHealth(t *testing.T) {
	s := NewServer(&fakeService{}, &fakeWorker{})
	for _, r := range s.Routes() {
		if r.Pattern == "GET /health" || r.Pattern == "/health" {
			t.Errorf("health should be provided by the gateway, not httpapi: %s", r.Pattern)
		}
	}
}
