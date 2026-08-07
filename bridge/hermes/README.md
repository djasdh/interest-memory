# interest-memory — Hermes bridge plugin

Hermes `MemoryProvider` adapter that connects conversations to the local
interest-memory Go service:

- **session-start recall** — `prefetch()` calls `GET /api/v1/{agent}/recall`
  and returns bare text; Hermes re-wraps it in `<memory-context>`.
- **session-end ingest** — `on_session_end()` posts the full transcript to
  `POST /api/v1/{agent}/sessions`; the backend worker runs the fork →
  verify → interest → wiki → rebuild pipeline asynchronously.

The backend does all memory work (design §五); this plugin is a thin REST
adapter with in-memory turn buffering.

## Deploy

1. Copy this directory to the Hermes user plugins path:

   ```bash
   mkdir -p "$HERMES_HOME/plugins/interest"
   cp -r bridge/hermes/. "$HERMES_HOME/plugins/interest/"
   ```

   (`$HERMES_HOME` defaults to `~/.hermes`; the plugin directory is
   `$HERMES_HOME/plugins/<name>/`, NOT `plugins/memory/`.)

2. Enable it in `$HERMES_HOME/config.yaml`:

   ```yaml
   memory:
     provider: interest
   ```

3. Configure the service URL in `$HERMES_HOME/.env`:

   ```
   INTEREST_BASE_URL=http://127.0.0.1:8899
   ```

   Optional: `INTEREST_AGENT=<namespace>` overrides the agent namespace
   (defaults to the Hermes profile via `agent_identity`); `INTEREST_TIMEOUT`
   sets the per-request timeout (default 8s).

4. Start the interest-memory service (see repo README), then start a new
   Hermes session.

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `INTEREST_BASE_URL` | `http://127.0.0.1:8899` | interest-memory service URL (required) |
| `INTEREST_AGENT` | Hermes profile | agent namespace for `/api/v1/{agent}/...` (overrides profile; see "Namespace resolution") |
| `INTEREST_TIMEOUT` | `8` | per-request timeout seconds |

## How it works

- `is_available()` — env-only check (no network), so Hermes silently skips the
  provider when `INTEREST_BASE_URL` is unset.
- `initialize(session_id, **kwargs)` — resolves the agent namespace from
  `agent_identity` (Hermes profile); skips writes for non-primary contexts
  (cron/subagent) so system prompts never corrupt the user representation.
- `sync_turn(...)` — buffers each turn in memory; no per-turn HTTP (the
  backend's per-agent serialized worker + unprocessed-transcript retry covers
  durability).
- `on_session_end(messages)` — posts the buffered transcript (or the full
  history when provided) as `{session_id, turn_count, raw_turns}`.
- `get_tool_schemas()` — returns `[]` (context-only provider).

## Namespace resolution (special adapter — not a bug)

Unlike the other bridges (opencode / pi / codex / claudecode / reasonix, which
default their namespace to the platform name), Hermes has **no single fixed
platform name**. Namespace resolves with this precedence:

```
INTEREST_AGENT env  >  Hermes active profile (agent_identity)  >  "default"
```

- This is a **special adapter choice**, not an architecture gap: Hermes is
  profile-oriented (`hermes profile use coder`), so per-profile memory
  namespaces are the intended isolation unit.
- Consequence: with the built-in `default` profile and no `INTEREST_AGENT`,
  Hermes shares the `"default"` namespace with any other bridge that also
  falls back to `"default"` (e.g. openclaw before you set
  `plugins.entries.interest-memory.config.agent`). To keep Hermes isolated,
  set `INTEREST_AGENT=hermes` (or a profile-specific value) in
  `~/.hermes/.env`.

## Smoke test without Hermes

```bash
# Start the service
cd interest-memory && go run ./cmd/server

# Ingest a transcript
curl -s -X POST http://127.0.0.1:8899/api/v1/agent/myagent/sessions \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"s1","turn_count":2,"raw_turns":"[{\"role\":\"user\",\"content\":\"prefer golang\"}]"}'

# Recall (bare text)
curl -s "http://127.0.0.1:8899/api/v1/agent/myagent/recall?query=golang"
```
