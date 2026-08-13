# interest-memory — agent bridges

Bridge plugins connecting AI coding agents to the local interest-memory REST
service. Each bridge implements three capabilities:

1. **Session-start recall** — `GET /api/v1/{agent}/recall?query=<last user text>`
   injected into the model context.
2. **Session-end ingest** — full transcript posted to
   `POST /api/v1/{agent}/sessions` (with `session_id`, `turn_count`,
   `raw_turns`, `session_date`).
3. **Consumer tools** — `memory_search` (`GET /search`) and `memory_logs`
   (`GET /logs`).

All failures are silent: a down service never blocks a session. When
`INTEREST_BASE_URL` is unset the bridge degrades to tools-only.

## Environment variables (all bridges)

| Var | Default | Purpose |
|---|---|---|
| `INTEREST_BASE_URL` | `http://127.0.0.1:8899` | interest-memory service URL |
| `INTEREST_AGENT` | `opencode` / `default` / `pi` | agent namespace for `/api/v1/{agent}/...` |
| `INTEREST_TIMEOUT` | `8` | per-request timeout seconds |

## Bridges

| Agent | Source | Deploy to | Form |
|---|---|---|---|
| Hermes | `bridge/hermes/` | `$HERMES_HOME/plugins/interest/` | MemoryProvider plugin |
| opencode | `bridge/opencode/memory.ts` | `~/.config/opencode/plugin/memory.ts` | local plugin (`{id, server}`) |
| openclaw | `bridge/openclaw/interest-memory/` | `<configDir>/extensions/interest-memory/` | native plugin |
| pi | `bridge/pi/memory.ts` | `~/.pi/agent/extensions/interest-memory/index.ts` | TS extension |
| claudecode | `bridge/claudecode/` | `claude --plugin-dir bridge/claudecode` | plugin (`.claude-plugin/plugin.json`) |
| codex | `bridge/codex/` | `~/.codex/hooks.json` + `~/.codex/config.toml` | plugin (`.codex-plugin/plugin.json`) / hooks |
| reasonix | `bridge/reasonix/` | `reasonix plugin install bridge/reasonix --link --yes` | plugin (`reasonix-plugin.json`) |
| DSH | `bridge/dsh/` | Cordis row (`@djasdh/interest-memory-dsh-bridge`) | Cordis plugin (`npm` package) |

## MCP server (codex / claudecode / reasonix)

`bridge/mcp-server/` is a shared Node MCP server exposing the consumer tools
(`memory_search`, `memory_logs`) over MCP stdio. The agent namespace comes
from `INTEREST_AGENT` in each client's MCP config.

```bash
cd bridge/mcp-server && npm install   # @modelcontextprotocol/sdk + zod
```

```jsonc
// per-client MCP config
{ "command": "node", "args": ["/abs/path/bridge/mcp-server/server.ts"],
  "env": { "INTEREST_AGENT": "codex" /* claudecode | reasonix */ } }
```

Note: `server.ts` uses the SDK 1.30 `registerTool(name, config, cb)` signature
(the old `(name, description, schema, cb)` form breaks tool calls with
`typedHandler is not a function`).

## Hermes

MemoryProvider plugin, the reference implementation.

- **Recall** — `prefetch()` calls `GET /api/v1/{agent}/recall`, returns bare
  text; Hermes re-wraps it in `<memory-context>`.
- **Ingest** — `on_session_end()` posts the full transcript to
  `POST /api/v1/{agent}/sessions`; the backend runs fork → verify → interest →
  wiki → rebuild asynchronously.
- **Tools** — `memory_search`, `memory_logs`.

## opencode

```bash
mkdir -p ~/.config/opencode/plugin
cp bridge/opencode/memory.ts bridge/opencode/memory-lib.ts ~/.config/opencode/plugin/
```

- **Recall** — `experimental.chat.messages.transform`, once per user turn,
  deduped by message id. The recall turn is spliced **in place** into the
  `output.messages` array opencode passes to the model (replacing
  `output.messages` is silently dropped). The ingest cache snapshots messages
  **before** the splice, so injected context is never ingested back.
- **Ingest** — on `session.status` idle (debounced 2s) and `session.deleted`.
- **Tools** — `memory_search`, `memory_logs` via `tool` hooks.
- **Note** — `messages.transform` is undocumented (experimental, present in
  `@opencode-ai/plugin` types). It is the only hook satisfying per-turn dynamic
  injection without polluting history; if an upgrade removes it, recall
  silently no-ops (failure isolation keeps sessions working).

## openclaw

```bash
mkdir -p ~/.openclaw/extensions
cp -r bridge/openclaw/interest-memory ~/.openclaw/extensions/interest-memory/
(cd ~/.openclaw/extensions/interest-memory && npm install --no-audit --no-fund)
```

Enable in `openclaw.json` (requires plugin API `>=2026.7.1`):

```jsonc
{ "plugins": { "entries": { "interest-memory": {
    "enabled": true,
    "config": { "agent": "openclaw" },
    "hooks": { "allowConversationAccess": true, "allowPromptInjection": true }
} } } }
```

- **Recall** — `before_prompt_build` (`prependContext`), deduped by prompt text.
- **Ingest** — `before_agent_finalize` caches the conversation per session id;
  `session_end` pushes it once, so multi-turn conversations produce a single
  transcript.
- **Tools** — named `interest_search` / `interest_logs` (not `memory_*`) to
  avoid collision with the bundled `memory-core` plugin.
- Optional config (`baseUrl`, `agent`, `timeoutMs`) overrides env.

## pi

```bash
mkdir -p ~/.pi/agent/extensions/interest-memory
cp bridge/pi/memory.ts ~/.pi/agent/extensions/interest-memory/index.ts
cp bridge/pi/lib.ts ~/.pi/agent/extensions/interest-memory/lib.ts
```

pi loads **every** `*.ts` file under `extensions/` as an extension, so the
entry must be named `index.ts` (a bare `lib.ts` would be mis-loaded).

- **Recall** — `before_agent_start` (hidden `customType` message,
  `display: false`).
- **Ingest** — `session_shutdown` (`ctx.sessionManager.getEntries()`).
- **Tools** — `memory_search`, `memory_logs` via `pi.registerTool`.
- `typebox` / `@earendil-works/pi-coding-agent` are provided via pi's virtual
  modules — no dependency install needed.

## claudecode

```bash
claude --plugin-dir /path/to/interest-memory/bridge/claudecode
```

- **Recall** — `UserPromptSubmit` hook (per turn; stdout injected).
- **Ingest** — `SessionEnd` hook (reads `transcript_path` jsonl).
- **Tools** — MCP `memory_search` / `memory_logs` from the plugin's `.mcp.json`,
  namespaced `mcp__plugin_interest-memory_interest-memory__*`; needs
  `--dangerously-skip-permissions` to bypass per-server approval.

## codex

Codex's plugin system is marketplace-driven; the packaged plugin lives in
`bridge/codex/.codex-plugin/plugin.json` for marketplace distribution. For a
local setup, register hooks and MCP directly:

```bash
cp bridge/codex/hooks/hooks.json ~/.codex/hooks.json   # adjust script paths to absolute
# MCP: append to ~/.codex/config.toml
#   [mcp_servers.interest-memory]
#   command = "node"
#   args = ["/abs/path/bridge/mcp-server/server.ts"]
#   env = { INTEREST_AGENT = "codex" }
```

- **Recall** — `UserPromptSubmit` hook (per turn; stdout injected).
- **Ingest** — `SessionEnd` hook (reads `~/.codex/sessions/*.jsonl`,
  `response_item` format); hook timeout budget is 1–3s.
- **Tools** — MCP `memory_search` / `memory_logs`; needs
  `--dangerously-bypass-approvals-and-sandbox` to reach localhost.
- Hooks only fire in interactive REPL sessions (headless `codex exec` does not
  run hooks).

## reasonix

```bash
reasonix plugin install /path/to/interest-memory/bridge/reasonix --link --replace --yes
```

- **Recall** — `UserPromptSubmit` hook (per turn; stdout injected).
- **Ingest** — `SessionEnd` hook (reads the session jsonl; `transcript_path`
  from the hook event, or newest file under `REASONIX_HOME`).
- **Tools** — MCP `memory_search` / `memory_logs`.
- Hooks only fire in interactive sessions (TUI / `reasonix serve` / desktop);
  headless `reasonix run` does not run hooks.

## Tests

Dependency-free (node:test + tiny HTTP stub; no agent runtime needed):

```bash
node --test bridge/opencode/memory-lib.test.mjs
node --test bridge/openclaw/interest-memory/lib.test.mjs
node --test bridge/pi/lib.test.mjs
node --test bridge/mcp-server/lib.test.mjs
node --test bridge/claudecode/hooks/lib.test.mjs
node --test bridge/codex/hooks/lib.test.mjs
node --test bridge/reasonix/hooks/lib.test.mjs
```

Each `lib` module is importable without the agent runtime, so pure logic
(transcript reduction, URL/payload shaping) is covered in isolation. End-to-end
service behavior is covered by `scripts/e2e.sh` (needs LLM keys).
