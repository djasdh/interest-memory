[English](README.en.md) | [中文](README.md)

![interest-memory](assets/banner.png)

# interest-memory — Long-term memory for AI agents

**One ~50MB process instead of a Postgres + Redis + vector DB stack.**

Agents forget everything between sessions. Not the model's fault — they lack a real memory layer. interest-memory is a standalone memory backend: at the end of a session it extracts interest points from the transcript, verifies and cleans them, and writes them into a local knowledge base; at the start of the next session it recalls and injects relevant context. The entire footprint: **one 18MB binary + one SQLite file**.

## Why

| | interest-memory | mem0 | Zep | Letta |
|---|---|---|---|---|
| Deploy | **single binary + SQLite** | Python + vector DB | Postgres stack | Postgres + per-agent processes |
| Idle RAM | **~17 MB** (measured) | hundreds of MB | hundreds of MB–1G+ | hundreds of MB+ |
| External deps | **none** (LLM via API) | vector DB required | Redis + Postgres | Postgres |
| Runs on | a Raspberry Pi | a real server | a real server | a real server |

Other memory systems need a server. This one needs a process.

**What it does**

- Session end: automatically extracts interest points → verifies → writes to the local knowledge base
- Session start: recalls relevant memories → injects into context (concise entries only, full content on demand, minimal context pollution)
- Multi-agent shared memory: one service for many agents (Hermes / OpenCode / Claude Code / Codex etc.), with isolated, fully-shared, or selective sharing
- Full audit: every structural change is written to `change_log`, replayable
- Every entry carries evidence (web URL / turn / query); subjective preferences are never stored as facts; contradictions are closed in a loop

## Quick start

**One-command install (curl)**

```bash
curl -fsSL https://raw.githubusercontent.com/djasdh/interest-memory/main/scripts/install.sh | bash
```

Auto-fetches the source → checks/installs dependencies → guides setup → optional systemd.

**Configure the LLM (let your agent fetch and run it)**

```bash
curl -fsSL https://raw.githubusercontent.com/djasdh/interest-memory/main/scripts/install_llm.py | python3 - --provider <provider>
# --help lists all providers; hand to your agent: it reads --help (its operating instructions) and configures itself
```

**Prebuilt binaries (optional)**: [Release v0.1.0](https://github.com/djasdh/interest-memory/releases) (linux / mac / windows)

## Highlights

- **Light** — ~17 MB idle, <75 MB peak (measured); all data in one local SQLite file
- **Self-hosted** — one binary + one config; no cloud dependency, fully localizable
- **Trustworthy** — 3-stage verification (check → relation verdict → evidence), subjectivity exemption, contradiction loop
- **Controllable** — full `change_log` audit; per-agent namespaces (isolated/shared); progressive disclosure keeps context clean
- **Integrates** — Hermes plugin out of the box; any other agent via REST API

## Resource usage (measured)

| Metric | Value |
|---|---|
| Binary size | ~18 MB (cgo static sqlite-vec) |
| Idle memory | **~17 MB RSS** (measured) |
| Pipeline peak | <75 MB RSS |
| Initial footprint | ~20 MB (binary + empty DB) |
| Growth | ~38 MB after a week of use; mostly raw session transcripts (~71%) |

`session_transcripts` keeps full raw text — trim externally to bound disk growth; `fork.max_concurrency` / `verify.max_concurrency` cap peak memory.

## Integration

Multiple agent frameworks are supported out of the box, sharing one env set (`INTEREST_BASE_URL` / `INTEREST_AGENT` / `INTEREST_TIMEOUT`); a down service never blocks a session:

| Agent | Form |
|---|---|
| Hermes | MemoryProvider plugin (`$HERMES_HOME/plugins/interest/`) |
| opencode | local plugin (`~/.config/opencode/plugin/memory.ts`) |
| openclaw | native plugin (`<configDir>/extensions/interest-memory/`) |
| pi | TS extension (`~/.pi/agent/extensions/interest-memory/`) |
| claudecode / codex / reasonix | official plugin + shared MCP server (`bridge/mcp-server/`) |

Every bridge offers the same capabilities: session-start recall injection, session-end transcript push, and `memory_search` / `memory_logs` consumer tools. See `bridge/README.md`.

## Architecture

```
internal/store/      SQLite (interest points/wiki pages/edges/claims/transcripts/change_log)
internal/vec/        sqlite-vec vector index (FTS fallback)
internal/llm/        OpenAI-compatible Chat/Embedding
internal/fork/       sliding-window split + parallel candidate extraction
internal/verify/     3-stage verification (check/claims/contradictions)
internal/wiki/       per-point agent-loop writer + related-page reconciliation
internal/recall/     recall injection + structured queries
bridge/hermes/       Hermes MemoryProvider plugin
```

## Docs

- **REST API** — `POST /api/v1/{agent}/sessions`, `GET /api/v1/{agent}/recall`, `search` / `logs` / `stats` / `jobs` (table below)
- **Config** — fully commented `config.example.yaml` (llm / embedding / fork / verify / wiki / recall / namespaces)
- **Development** — `CGO_ENABLED=1 go test -race ./...`; plugin tests `node --test bridge/...`; e2e `bash scripts/e2e.sh`

### API quick reference

| Method | Path | Description |
|---|---|---|
| POST | `/api/v1/{agent}/sessions` | session-end transcript push → 202 job_id |
| GET | `/api/v1/{agent}/recall?query=&after=&before=&days=` | recall injection (optional time filters) |
| GET | `/api/v1/{agent}/search?query= or ?id=&top_k=` | consumer query: full content + edges |
| GET | `/api/v1/{agent}/logs?limit=&offset=` | change log (desc, paged) |
| GET | `/api/v1/{agent}/interest-points` | list interest points |
| GET | `/api/v1/{agent}/wiki/pages[?type=]` | list wiki pages |
| POST | `/api/v1/{agent}/fork` | manually trigger forking |
| GET | `/api/v1/{agent}/jobs/{id}` | job status |
| GET | `/api/v1/{agent}/stats` | stats |
| GET | `/api/health` | health check |

### Namespaces

Each agent (`{agent}` path segment / `INTEREST_AGENT`) has an isolated namespace; cross-namespace reads are configured via `namespaces`:

```yaml
namespaces:
  mode: isolated   # isolated (default) | all | custom
  visible_to:      # custom only: one-way visibility declarations
    codex: [opencode, pi]
```

Shared results are annotated with origin (`[from: <agent>]` on recall lines, `result.agent` in search/get).

## Dependencies

`my-agent-core`, mattn/go-sqlite3 (cgo static), sqlite-vec, goldmark-obsidian (wikilinks). All MIT-compatible.

## License

[MIT](LICENSE) — Contributions are welcome whether written by a human or an AI — quality is what counts.
