package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/service"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/worker"
)

// recordingWorker implements worker.Worker and records whether Enqueue was
// ever called — the gate drops excluded boards before the worker queue, so a
// call here would mean the exclusion failed. Embedding / fork extraction /
// token accounting all live downstream of Enqueue; a skipped Enqueue is the
// proof that none of them can run for this session.
type recordingWorker struct {
	enqueued  bool
	enqAgent  string
	enqSessID string
}

func (r *recordingWorker) Enqueue(_ context.Context, agentID, sessionID string) (string, error) {
	r.enqueued = true
	r.enqAgent = agentID
	r.enqSessID = sessionID
	return "job-1", nil
}
func (r *recordingWorker) GetJob(context.Context, string) (*worker.Job, error) { return nil, nil }

// newIngestHarness wires the real ingest chain: real config (drives the
// kanban_exclude matching), real SQLite store (proves nothing is persisted),
// a minimally-wired service (the exclusion gate needs no LLM/embedding, so
// those dependencies are nil), and a recording worker.
func newIngestHarness(t *testing.T, excludes []string) (*httptest.Server, *store.SQLiteStore, *recordingWorker) {
	t.Helper()
	cfg := config.Default()
	cfg.InterestMemory.KanbanExclude = excludes
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc := service.New(cfg, st, nil, nil, nil)
	rw := &recordingWorker{}
	ts := httptest.NewServer(NewServer(svc, rw).Handler())
	t.Cleanup(ts.Close)
	return ts, st, rw
}

func postIngest(t *testing.T, ts *httptest.Server, body string) (int, map[string]string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/v1/agent-a/sessions", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// transcriptCount queries session_transcripts directly for the agent — the
// storage-level proof that an excluded board left no row.
func transcriptCount(t *testing.T, st *store.SQLiteStore, agentID string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM session_transcripts WHERE agent_id = ?`, agentID).Scan(&n); err != nil {
		t.Fatalf("count transcripts: %v", err)
	}
	return n
}

func TestKanbanIngestExcludedByIDPersistsNothing(t *testing.T) {
	// 验收 1：配置 kanban_exclude 按 ID 排除——被排除看板的 mock 导入
	// 不存储（session_transcripts 无行）、不 embedding（interest_points 无产物）、
	// 不计 token（usage 表空）、不进入 worker 队列。
	ts, st, rw := newIngestHarness(t, []string{"default"})

	code, out := postIngest(t, ts, `{"session_id":"s1","turn_count":3,"raw_turns":"[{\"role\":\"user\",\"content\":\"hi\"}]","kanban_board":"default","kanban_board_name":"Default"}`)
	if code != http.StatusAccepted || out["skipped"] != "kanban_board_excluded" {
		t.Fatalf("status=%d out=%v, want 202 + skipped=kanban_board_excluded", code, out)
	}

	if n := transcriptCount(t, st, "agent-a"); n != 0 {
		t.Errorf("excluded board left %d transcript rows, want 0", n)
	}
	if rw.enqueued {
		t.Errorf("excluded board reached the worker queue (agent=%q session=%q)", rw.enqAgent, rw.enqSessID)
	}
	pts, err := st.ListInterestPoints(context.Background(), "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 0 {
		t.Errorf("excluded board produced %d interest points (embedding side-effects), want 0", len(pts))
	}
	rows, err := st.ListUsage(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("excluded board recorded token usage %v, want 0 rows", rows)
	}
}

func TestKanbanIngestExcludedByName(t *testing.T) {
	// 验收：按显示名称匹配——slug 不在列表但名称命中，同样跳过。
	ts, st, rw := newIngestHarness(t, []string{"Default"})

	code, out := postIngest(t, ts, `{"session_id":"s2","turn_count":2,"raw_turns":"[]","kanban_board":"other-slug","kanban_board_name":"Default"}`)
	if code != http.StatusAccepted || out["skipped"] != "kanban_board_excluded" {
		t.Fatalf("status=%d out=%v, want 202 + skipped=kanban_board_excluded (name match)", code, out)
	}
	if n := transcriptCount(t, st, "agent-a"); n != 0 {
		t.Errorf("name-excluded board left %d transcript rows, want 0", n)
	}
	if rw.enqueued {
		t.Error("name-excluded board reached the worker queue")
	}
}

func TestKanbanIngestPartialExclude(t *testing.T) {
	// 验收：部分排除——列表中看板被跳过，未列出的正常存储并入队。
	ts, st, rw := newIngestHarness(t, []string{"default"})

	code, out := postIngest(t, ts, `{"session_id":"s1","turn_count":3,"raw_turns":"[]","kanban_board":"default"}`)
	if code != http.StatusAccepted || out["skipped"] != "kanban_board_excluded" {
		t.Fatalf("excluded push: status=%d out=%v", code, out)
	}

	code, out = postIngest(t, ts, `{"session_id":"s2","turn_count":2,"raw_turns":"[{\"role\":\"user\",\"content\":\"keep me\"}]","kanban_board":"other"}`)
	if code != http.StatusAccepted || out["job_id"] == "" {
		t.Fatalf("kept push: status=%d out=%v, want 202 + job_id", code, out)
	}
	if n := transcriptCount(t, st, "agent-a"); n != 1 {
		t.Errorf("transcript rows = %d, want 1 (only the non-excluded board)", n)
	}
	if !rw.enqueued || rw.enqSessID != "s2" {
		t.Errorf("worker enqueued = %v session=%q, want enqueued s2", rw.enqueued, rw.enqSessID)
	}
}

func TestKanbanIngestUnconfiguredStoresNormally(t *testing.T) {
	// 验收 2（回归保护）：无配置（空列表）时行为与未配置完全一致——
	// 看板字段照常入库、照常入队，不触发任何排除。
	ts, st, rw := newIngestHarness(t, []string{})

	code, out := postIngest(t, ts, `{"session_id":"s1","turn_count":3,"raw_turns":"[]","kanban_board":"default"}`)
	if code != http.StatusAccepted || out["job_id"] == "" {
		t.Fatalf("status=%d out=%v, want 202 + job_id", code, out)
	}
	if n := transcriptCount(t, st, "agent-a"); n != 1 {
		t.Errorf("transcript rows = %d, want 1 (unconfigured must store normally)", n)
	}
	if !rw.enqueued || rw.enqSessID != "s1" {
		t.Errorf("worker enqueued = %v session=%q, want enqueued s1", rw.enqueued, rw.enqSessID)
	}
}
