# interest-memory — agent bridges

Bridge plugins connecting AI coding agents to the local interest-memory REST
service (design §五/§八). Each bridge implements three capabilities, mirroring
the original Hermes `MemoryProvider` (`bridge/hermes/`):

1. **Session-start recall** — `GET /api/v1/{agent}/recall?query=<last user text>`
   injected into the model context.
2. **Session-end ingest** — full transcript reduced to `[{role, content}]`
   posted to `POST /api/v1/{agent}/sessions` (with `session_id`, `turn_count`,
   `raw_turns`, `session_date`).
3. **Consumer tools** — `memory_search` (`GET /search`) and `memory_logs`
   (`GET /logs`).

## Environment variables (all bridges)

| Var | Default | Purpose |
|---|---|---|
| `INTEREST_BASE_URL` | `http://127.0.0.1:8899` | interest-memory service URL |
| `INTEREST_AGENT` | `opencode` / `default` / `pi` | agent namespace for `/api/v1/{agent}/...` |
| `INTEREST_TIMEOUT` | `8` | per-request timeout seconds |

All failures are silent (failure isolation): a down service never blocks a
session. When `INTEREST_BASE_URL` is unset the bridge degrades to tools-only
(recall/ingest are no-ops).

## Bridges

| Agent | Source | Deploy to | Form |
|---|---|---|---|
| Hermes | `bridge/hermes/` | `$HERMES_HOME/plugins/interest/` | MemoryProvider plugin |
| opencode | `bridge/opencode/memory.ts` | `~/.config/opencode/plugin/memory.ts` | local plugin (`{id, server}`) |
| openclaw | `bridge/openclaw/interest-memory/` | `<configDir>/extensions/interest-memory/` | native plugin |
| pi | `bridge/pi/memory.ts` | `~/.pi/agent/extensions/memory.ts` | TS extension |

## opencode

```bash
mkdir -p ~/.config/opencode/plugin
cp bridge/opencode/memory.ts ~/.config/opencode/plugin/memory.ts
# also copy the sibling memory-lib.ts next to it
cp bridge/opencode/memory-lib.ts ~/.config/opencode/plugin/memory-lib.ts
```

- Recall injection via `experimental.chat.messages.transform` (once per user
  turn, deduped by message id).
- Transcript push on `session.status` idle (debounced 2s) and `session.deleted`.
- Tools: `memory_search`, `memory_logs` (registered via `tool` hooks).

## openclaw

```bash
mkdir -p ~/.openclaw/extensions
cp -r bridge/openclaw/interest-memory ~/.openclaw/extensions/interest-memory/
# openclaw does NOT provide a typebox virtual module — install the plugin's
# own dependency (declared in package.json):
(cd ~/.openclaw/extensions/interest-memory && npm install --no-audit --no-fund)
```

Enable in `openclaw.json`:

```jsonc
{
  "plugins": {
    "entries": {
      "interest-memory": {
        "enabled": true,
        "hooks": {
          "allowConversationAccess": true,
          "allowPromptInjection": true
        }
      }
    }
  }
}
```

The plugin requires plugin API `>=2026.7.1` (verified against host
`2026.7.1-2`). Configure a model provider in the same `openclaw.json`
(deepseek is not in the built-in catalog by default):

```jsonc
{
  "models": {
    "providers": {
      "deepseek": {
        "api": "openai-completions",
        "apiKey": { "source": "env", "provider": "default", "id": "DEEPSEEK_API_KEY" },
        "baseUrl": "https://api.deepseek.com/v1",
        "models": [
          { "id": "deepseek-chat", "name": "DeepSeek Chat" },
          { "id": "deepseek-reasoner", "name": "DeepSeek Reasoner" }
        ]
      }
    }
  }
}
```

- Recall injection via `before_prompt_build` (`prependContext`), deduped by
  prompt text and reset on `agent_end` (the event carries no session id).
- Transcript push via `agent_end` (complete `messages` in the event);
  `session_end` only does cursor cleanup (2s drain budget — no heavy IO).
- Tools are named **`interest_search`** / **`interest_logs`** (not
  `memory_*`) so they never collide with the bundled `memory-core` plugin.
- Optional plugin config (`plugins.entries["interest-memory"].config`):
  `baseUrl`, `agent`, `timeoutMs` (overrides env).
- Known limitation: `agent_end` has no stable session id, so ingest uses the
  per-run `runId` — every agent run pushes an independent session transcript
  (multi-turn conversations produce one session per turn).

## pi

Deploy as a subdirectory with `index.ts` — pi loads **every** `*.ts` file
directly under `extensions/` as an extension, so a bare `lib.ts` would be
mis-loaded and fail:

```bash
mkdir -p ~/.pi/agent/extensions/interest-memory
cp bridge/pi/memory.ts ~/.pi/agent/extensions/interest-memory/index.ts
cp bridge/pi/lib.ts ~/.pi/agent/extensions/interest-memory/lib.ts
```

- Recall injection via `before_agent_start` (hidden `customType` message,
  `display: false`).
- Transcript push via `session_shutdown` (`ctx.sessionManager.getEntries()`,
  guaranteed complete — the aborted turn is settled first).
- Tools: `memory_search`, `memory_logs` (`pi.registerTool`).
- `typebox` / `@earendil-works/pi-coding-agent` are provided to extensions via
  pi's virtual modules — no dependency install needed.

## Tests

Dependency-free (node:test + tiny HTTP stub; no agent runtime needed):

```bash
node --test bridge/opencode/memory-lib.test.mjs
node --test bridge/openclaw/interest-memory/lib.test.mjs
node --test bridge/pi/lib.test.mjs
```

Each `lib` module is importable without the agent runtime, so the pure logic
(transcript reduction, URL/payload shaping) is covered in isolation. End-to-end
service behavior is covered by `scripts/e2e.sh` (needs LLM keys).
