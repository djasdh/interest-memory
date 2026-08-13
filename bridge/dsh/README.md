# @djasdh/interest-memory-dsh-bridge

interest-memory ↔ **DeepSeek Harness (DSH)** bridge. Gives DSH agents a real
long-term memory layer:

- **Zero-lag recall injection** — on every user message the bridge fetches
  `/api/v1/{agent}/recall` with that message and injects the memory as an
  **owned runtime-context snapshot** in the *same* model step (no phantom user
  messages, no system-prompt churn, stable KV-cache prefix).
- **Session-end ingest** — the session transcript is pushed to
  `POST /api/v1/{agent}/sessions` once the session ends (owned snapshots
  filtered out, so the injected memory is never ingested back).
- **Tools** — `memory_search` / `memory_logs` / `memory_ingest` (force a
  checkpoint for long-running sessions).

Failure isolated: a dead interest-memory service never blocks a turn
(timeouts degrade to "no memory").

## Install

DSH ships a pnpm-forwarding CLI for profile plugins. With the interest-memory
service already running (see the repo root README), a user installs the
package into their profile:

```bash
dsh plugin --profile web add @djasdh/interest-memory-dsh-bridge
```

`dsh plugin` forwards to pnpm inside the profile directory
(`$DSH_HOME/profiles/<name>/`), so the package lands in the profile's
`node_modules` and the loader resolves the row's `name:` from there. The
package is plain JS — no build step, no `allowBuilds` entry needed.

## Mount

### As an agent preset row (per-session memory — recommended)

Add the row to a preset's `agent.cordis.yml`, then start sessions from that
preset:

```yaml
- id: interest-memory
  name: '@djasdh/interest-memory-dsh-bridge'
  config:
    baseUrl: http://127.0.0.1:8899   # optional (default shown)
    agent: dsh                        # optional; the interest-memory namespace
```

Copying a shipped preset (e.g. `standard` or `cordis`) and adding this row is
the fastest path.

### As a host row (shared service across sessions)

Add to the profile patch, e.g. `~/.dsh/profiles/web/cordis.patch.yml`:

```yaml
- insert:
    - id: interest-memory
      name: '@djasdh/interest-memory-dsh-bridge'
      config:
        baseUrl: http://127.0.0.1:8899
        agent: dsh
```

Recall state is keyed **per agent**, so one host row serves every session
(each agent id maps to its interest-memory namespace).

## Config

| field | default | meaning |
|---|---|---|
| `baseUrl` | `http://127.0.0.1:8899` | interest-memory service URL |
| `agent` | `dsh` | interest-memory namespace (`/api/v1/{agent}/...`) |
| `recallTimeoutMs` | `5000` | per-recall HTTP timeout (never blocks a turn longer than this) |
| `ingestTimeoutMs` | `15000` | per-ingest HTTP timeout |
| `traceDir` | `''` | optional directory for JSON traces (empty = off) |

## How recall works

DSH's turn loop assembles the system prompt *before* the current user message
is available to plugins, which makes naive "inject at request time" land one
step late. The bridge uses two seams instead:

1. `agent/inbox/claimed` fires at message-claim time, *before* assembly — the
   bridge starts the recall fetch with the **current** message there.
2. `system-prompt/assemble` waterfall awaits the in-flight fetch and pushes
   the memory as a **context contribution**. The DSH runtime-context
   projection then materialises it as an *owned* user-role snapshot
   (`source.kind='plugin'`, `@deepseek-ai/dsh-system-prompt`) appended after
   the user message — zero lag, and the snapshot is reused afterwards so the
   prefix stays byte-stable.

## Development

```bash
npm test                       # node --test lib/  (pure helpers, no DSH runtime)
```

The plugin itself is a plain Cordis plugin (`{ name, inject, Config, apply }`)
— the same shape as the shipped `@deepseek-ai/dsh-*` packages.
