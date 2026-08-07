#!/usr/bin/env bash
# End-to-end test: pushes a transcript through the full memory pipeline
# (fork → verify#1 → interest → verify#2 → wiki agent loop → rebuild → recall)
# against the real service with real LLM providers.
#
# Providers:
#   LLM:        OpenAI-compatible (DeepSeek etc.) — requires LLM_API_KEY
#   embedding:  SiliconFlow BAAI/bge-m3 (free) — requires SILICONFLOW_API_KEY
#
# Usage: bash scripts/e2e.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${IM_E2E_PORT:-18999}"
DB_PATH="${IM_E2E_DB:-$(mktemp -u /tmp/im-e2e-XXXXXX.db)}"
CONFIG="${IM_E2E_CONFIG:-$(mktemp -u /tmp/im-e2e-XXXXXX.yaml)}"
LOG="${IM_E2E_LOG:-/tmp/im-e2e-server.log}"
BASE="http://127.0.0.1:${PORT}"
PID=""

die() { echo "FAIL: $*" >&2; exit 1; }
log() { echo "[e2e] $*"; }

cleanup() {
  if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  if [[ -z "${IM_E2E_KEEP_DB:-}" ]]; then
    rm -f "$DB_PATH" "$CONFIG"
  else
    echo "[e2e] keeping db=$DB_PATH config=$CONFIG (IM_E2E_KEEP_DB)"
  fi
}
trap cleanup EXIT

# ---- 1. key checks ---------------------------------------------------------
[[ -n "${LLM_API_KEY:-}" ]] || die "LLM_API_KEY not set"
[[ -n "${SILICONFLOW_API_KEY:-}" ]] || die "SILICONFLOW_API_KEY not set"

# ---- 2. temporary config ---------------------------------------------------
cat > "$CONFIG" <<EOF
server:
  host: 127.0.0.1
  port: $PORT
  db_path: $DB_PATH
llm:
  base_url: https://api.deepseek.com/v1
  api_key_env: LLM_API_KEY
  model: deepseek-chat
  max_tokens: 4096
embedding:
  base_url: https://api.siliconflow.cn/v1
  api_key_env: SILICONFLOW_API_KEY
  model: BAAI/bge-m3
  dimensions: 1024
fork:
  prefix_step: 5
  max_windows: 8
  max_concurrency: 4
  similarity_merge: 0.85
  similarity_relate: 0.50
  importance_boost_per_seen: 0.05
  max_candidates_per_window: 20
  min_confidence: 0.3
verify:
  use_web_search: true
  search_max: 5
  web_tool: myagent
  max_concurrency: 4
  min_confidence: 0
  sim_threshold: 0.45
  max_candidates: 30
wiki:
  max_hops: 3
  batch_size: 10
search:
  top_k: 3
  max_body_len: 4000
log:
  retain: 0
recall:
  top_k: 8
  include_wiki: true
  min_score: 0.30
EOF

# ---- 3. build + start server ----------------------------------------------
log "building service..."
(cd "$REPO" && go build -o /tmp/im-e2e-server ./cmd/server) || die "build failed"

log "starting server on :$PORT (db=$DB_PATH)"
/tmp/im-e2e-server -config "$CONFIG" >"$LOG" 2>&1 &
PID=$!

# ---- 4. wait for health ----------------------------------------------------
for _ in $(seq 1 30); do
  if curl -sf "$BASE/api/health" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -sf "$BASE/api/health" >/dev/null 2>&1 || die "server did not become healthy (log: $LOG)"

log "server healthy"

# ---- 5. push a transcript ---------------------------------------------------
# A short multi-turn conversation with clear technical decisions + preferences
# so the fork step has extractable interest points.
TRANSCRIPT='[
  {"role":"user","content":"我们团队在用 Go 做后端，我强烈倾向于用 PostgreSQL 而不是 MySQL，因为事务和 JSONB 都更好"},
  {"role":"assistant","content":"明白了，PostgreSQL 作为默认数据库，JSONB 用于半结构化数据。"},
  {"role":"user","content":"还有错误处理：我坚持所有外部调用都要 wrapped error，不要吞掉底层错误"},
  {"role":"assistant","content":"好的，统一用 %w 包裹，保留调用栈。"},
  {"role":"user","content":"对了，日志我喜欢结构化 JSON 格式，方便采集"},
  {"role":"assistant","content":"结构化日志记下来了。"}
]'

log "pushing transcript..."
RESP=$(curl -sf -X POST "$BASE/api/v1/agent-a/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"e2e-session-1\",\"turn_count\":6,\"raw_turns\":$(printf '%s' "$TRANSCRIPT" | python3 -c 'import sys,json;print(json.dumps(sys.stdin.read()))')}")
JOB_ID=$(printf '%s' "$RESP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["job_id"])')
[[ -n "$JOB_ID" ]] || die "no job_id from /sessions (resp: $RESP)"
log "job_id=$JOB_ID"

# ---- 6. poll job ------------------------------------------------------------
POLL_TIMES="${IM_E2E_POLL_TIMES:-90}"   # ×2s = default 180s; longer pipelines override
STATUS="queued"
for _ in $(seq 1 "$POLL_TIMES"); do
  JOB=$(curl -sf "$BASE/api/v1/agent-a/jobs/$JOB_ID" || echo '{"status":"lost"}')
  STATUS=$(printf '%s' "$JOB" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))')
  if [[ "$STATUS" == "done" ]]; then
    break
  fi
  if [[ "$STATUS" == "failed" ]]; then
    echo "job failed:" >&2
    printf '%s\n' "$JOB" | python3 -m json.tool >&2
    echo "--- server log tail ---" >&2
    tail -40 "$LOG" >&2 || true
    die "pipeline job failed"
  fi
  sleep 2
done
[[ "$STATUS" == "done" ]] || die "job timed out (status=$STATUS)"

log "pipeline job done"

# ---- 7. assert outputs -------------------------------------------------------
log "checking outputs..."

STATS=$(curl -sf "$BASE/api/v1/agent-a/stats")
IP_COUNT=$(printf '%s' "$STATS" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("interest_points",0))')
PG_COUNT=$(printf '%s' "$STATS" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("wiki_pages",0))')
echo "stats: $STATS"

[[ "$IP_COUNT" -gt 0 ]] || die "expected >0 interest_points"
[[ "$PG_COUNT" -gt 0 ]] || die "expected >0 wiki_pages (agent loop should write pages)"

# recall must return a non-empty memory_context (JSON-wrapped, no <memory-context> fence)
RECALL=$(curl -sf "$BASE/api/v1/agent-a/recall?query=PostgreSQL" 2>/dev/null || curl -sf --get "$BASE/api/v1/agent-a/recall" --data-urlencode "query=PostgreSQL")
CTX=$(printf '%s' "$RECALL" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("memory_context",""))')
echo "recall query=PostgreSQL -> ${CTX:0:200}"
[[ -n "$CTX" ]] || die "recall returned empty memory_context"

log "ALL CHECKS PASSED"
