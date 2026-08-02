English | [中文](README.md)

# interest-memory — Interest-Point Memory Service

A standalone Go memory service: at session end it extracts interest points from a conversation, verifies/cleans them, and writes them into a wiki with bidirectional links; at session start it recalls and injects context via RAG. Provides long-term memory for consumer agents such as Hermes.

## Features

- **Prefix-window parallel extraction** — one prefix-window step per 5 user turns (no split below 5), extracted in parallel, hitting DeepSeek/SiliconFlow prefix caching
- **Three-stage correction** — subjective points skip web fact-checking; new candidates are judged against the most similar historical point (supersede/update/delete → archive/merge/create); evidence is located to web URL / conversation turns / search query
- **Per-interest-point agent loop** — one loop per point with evidence + matching dialog segment; tools `wiki_query / wiki_tags / verify_claims / review / wiki_write` (web audit + read-only review + tag taxonomy)
- **Related-page reconciliation** — after writes/archives, a ≤3-hop graph propagation uniformly handles cascade archiving (page → superseded), contradiction closure, and content sync, batched at >10 pages
- **Consumer querying** — lean `recall` injection plus `memory_search` (full content + edges) and `memory_logs` tools
- **Temporal support** — `session_date` passthrough → event time (EventTime) → recall time filtering (after/before/last N days), supporting LongMemEval temporal evaluation
- **Full audit trail** — change_log records every structural change (title + action + structural edges); tag taxonomy (`ListTags` aggregation + `wiki_tags` tool)

## Architecture & Data Flow

```
Session end POST /sessions (Hermes pushes transcript incl. session_date)
  → worker serial (per agent)
  → fork      prefix windows (per 5 user turns) → parallel side-LLM extraction + dedup
  → verify#1  parallel check: subjective skip-web / relation verdict / evidence locators
  → interest  archive/merge/create by relation (EventTime/TurnRange persisted)
  → verify#2  claim extraction + contradiction detection
  → wiki      per-interest-point agent loop (verify_claims → review → wiki_write)
  → RebuildEdges  wikilink adjacency rebuild
  → reconcile related-page sync (≤3 hops: cascade / contradiction / content)

Session start GET /recall?query=
  → embed → vec retrieval → time filter (after/before/days) → lean injection (with (at date) stamp)

Consumer tools: memory_search (query/id + full content + edges) / memory_logs (change log)
```

## Quick Start

**Prerequisites**: Go 1.22+, CGO (static sqlite-vec), DeepSeek + SiliconFlow API keys.

```bash
# 1. Build (single binary)
go build ./cmd/server

# 2. Configure
cp config.example.yaml config.yaml
# Edit config.yaml: LLM (default DeepSeek), embedding (default SiliconFlow BAAI/bge-m3)
# Provide keys via environment variables
export DEEPSEEK_API_KEY=...     # LLM extraction / verification / writing
export SILICONFLOW_API_KEY=...  # embedding (BAAI/bge-m3, 1024 dims)

# 3. Run (default :8899)
./server -config config.yaml

# 4. End-to-end test (real LLMs)
bash scripts/e2e.sh
```

## REST API

| Method | Path | Description |
|---|---|---|
| POST | `/api/v1/{agent}/sessions` | Push session-end transcript (optional `session_date` RFC3339) → 202 job_id |
| GET | `/api/v1/{agent}/recall?query=&after=&before=&days=` | Recall injection (optional time filter) |
| GET | `/api/v1/{agent}/search?query= or ?id=&top_k=` | Consumer query: full content + edges |
| GET | `/api/v1/{agent}/logs?limit=&offset=` | Change log (newest-first, paginated) |
| GET | `/api/v1/{agent}/interest-points` | List interest points |
| GET | `/api/v1/{agent}/wiki/pages[?type=]` | List wiki pages |
| POST | `/api/v1/{agent}/fork` | Manually trigger fork |
| GET | `/api/v1/{agent}/jobs/{id}` | Job status |
| GET | `/api/v1/{agent}/stats` | Statistics |
| GET | `/api/health` | Health check |

## Hermes Integration

Install the plugin into the Hermes plugins directory:

```bash
cp -r bridge/hermes $HERMES_HOME/plugins/interest/
```

Env config: `INTEREST_BASE_URL` (default `http://127.0.0.1:8899`), `INTEREST_AGENT` (agent namespace, default profile), `INTEREST_TIMEOUT`.

Capabilities: session-start `prefetch` recall injection, session-end transcript push (incl. `session_date`), consumer `memory_search` / `memory_logs` tools.

## Configuration Reference

See `config.example.yaml`. Key sections:

- `server` — listen address / port / SQLite path
- `llm` — side LLM (extraction/verification/writing), independent base_url/api_key_env/model
- `embedding` — independently configurable, default SiliconFlow `BAAI/bge-m3` (1024 dims)
- `fork` — prefix window step(5) / max(8) / concurrency(4) / similarity thresholds
- `verify` — web search toggle / search_max / web_tool / concurrency
- `wiki` — reconciliation depth(3) / batch(10)
- `search` — consumer query top_k(3) / max_body_len(4000)
- `log` — change-log retention (0 = unlimited)
- `recall` — top_k(8) / include_wiki / min_score(0.30)

## Directory Layout

```
cmd/server/          entry point (-config flag, wiring + graceful shutdown)
internal/config/     YAML + env-override configuration
internal/store/      SQLite (interest points / wiki pages / edges / claims / transcripts / change_log)
internal/vec/        sqlite-vec vector index (FTS fallback)
internal/llm/        OpenAI-compatible Chat/Embedding
internal/fork/       prefix-window split + parallel candidate extraction
internal/verify/     three-stage correction (verification/claims/contradictions/recall grading)
internal/interest/   cleaning (merge/relate/create/archive)
internal/wiki/       write agent loop (5 tools) + related-page reconciliation
internal/recall/     recall injection + structured query
internal/service/    orchestration layer
internal/worker/     per-agent serial queue
internal/httpapi/    REST endpoints
internal/websearch/  registerable web-tool registry
bridge/hermes/       Hermes MemoryProvider plugin
```

## Testing

```bash
# Go full suite (with race detector)
CGO_ENABLED=1 go test -race ./...

# Hermes plugin
python3 bridge/hermes/test_interest.py

# End-to-end (requires DEEPSEEK_API_KEY + SILICONFLOW_API_KEY)
bash scripts/e2e.sh
```

## Dependencies

`my-agent-core` (local extracted library, `replace => ../my-agent-core`), mattn/go-sqlite3 (cgo static link), sqlite-vec, goldmark-obsidian (wikilink parsing).
