# interest-memory — agent bridges

Bridge plugins connecting AI coding agents to the local interest-memory REST
service (design §5/§8). Each bridge implements three capabilities, mirroring
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
| pi | `bridge/pi/memory.ts` | `~/.pi/agent/extensions/interest-memory/index.ts` | TS extension |
| claudecode | `bridge/claudecode/` | `claude --plugin-dir bridge/claudecode` | plugin (`.claude-plugin/plugin.json`) |
| codex | `bridge/codex/` | `~/.codex/hooks.json` + `~/.codex/config.toml` | plugin (`.codex-plugin/plugin.json`) / hooks |
| reasonix | `bridge/reasonix/` | `reasonix plugin install bridge/reasonix --link --yes` | plugin (`reasonix-plugin.json`) |

## MCP server (codex / claudecode / reasonix)

`bridge/mcp-server/` is a shared Node MCP server exposing the consumer tools
(`memory_search`, `memory_logs`) over MCP stdio. All three MCP clients point at
it; the agent namespace comes from `INTEREST_AGENT` set in each client's MCP
config.

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

## claudecode

```bash
claude --plugin-dir /path/to/interest-memory/bridge/claudecode
# or install via a marketplace / ~/.claude/plugins
```

- Recall injection via `UserPromptSubmit` hook (per turn; stdout injected).
- Transcript push via `SessionEnd` hook (reads `transcript_path` jsonl).
- Tools: MCP `memory_search` / `memory_logs` from the plugin's `.mcp.json`.
- Plugin MCP tools are namespaced `mcp__plugin_interest-memory_interest-memory__*`
  and need per-server approval (use `--dangerously-skip-permissions` to bypass).
- Verified: `claude -p` fires SessionEnd and UserPromptSubmit hooks end-to-end.

## codex

Codex's plugin system is marketplace-driven (a marketplace root manifest is
required for local install); the packaged plugin lives in
`bridge/codex/.codex-plugin/plugin.json` for marketplace distribution. For a
local setup, register the same hooks and MCP directly:

```bash
# hooks
cp bridge/codex/hooks/hooks.json ~/.codex/hooks.json   # adjust script paths to absolute
# MCP server
# append to ~/.codex/config.toml:
#   [mcp_servers.interest-memory]
#   command = "node"
#   args = ["/abs/path/bridge/mcp-server/server.ts"]
#   env = { INTEREST_AGENT = "codex" }
```

- Recall injection via `UserPromptSubmit` hook (per turn; stdout injected).
- Transcript push via `SessionEnd` hook (reads `~/.codex/sessions/*.jsonl`,
  `response_item` format); hook timeout budget is 1–3s.
- Tools: MCP `memory_search` / `memory_logs` (verified called from `codex exec`;
  needs `--dangerously-bypass-approvals-and-sandbox` to reach localhost).
- Hooks only fire in interactive REPL sessions (headless `codex exec` does not
  run hooks — same as reasonix `run`).

## reasonix

```bash
reasonix plugin install /path/to/interest-memory/bridge/reasonix --link --replace --yes
```

- Recall injection via `UserPromptSubmit` hook (per turn; stdout injected).
- Transcript push via `SessionEnd` hook (reads the Reasonix session jsonl;
  `transcript_path` from the hook event, or newest file under
  `REASONIX_HOME`).
- Tools: MCP `memory_search` / `memory_logs` (verified called from `reasonix run`).
- Hooks only fire in interactive sessions (TUI / `reasonix serve` / desktop) —
  headless `reasonix run` does not run hooks (verified).

## opencode

```bash
mkdir -p ~/.config/opencode/plugin
cp bridge/opencode/memory.ts ~/.config/opencode/plugin/memory.ts
# also copy the sibling memory-lib.ts next to it
cp bridge/opencode/memory-lib.ts ~/.config/opencode/plugin/memory-lib.ts
```

- Recall injection via `experimental.chat.messages.transform` (once per user
  turn, deduped by message id). The recall turn is spliced IN PLACE into the
  same `output.messages` array opencode passes to the model (the trigger
  contract is "mutate output in place" — replacing `output.messages` is
  silently dropped by opencode, see `session/prompt.ts`). The session-end
  ingest cache snapshots the messages BEFORE the splice, so the injected
  `<memory_context>` turn is never ingested back into the memory store.
- Transcript push on `session.status` idle (debounced 2s) and `session.deleted`.
- Tools: `memory_search`, `memory_logs` (registered via `tool` hooks).

**API stability note (2026-08 audit)**

`experimental.chat.messages.transform` is currently the **only** opencode
interface that can do per-turn dynamic recall injection, but be aware of its
official status:

- **Not in the official docs.** The opencode docs (`/docs/plugins/` event
  list, `/docs/rules/`, `/docs/config/`) only promise static instruction files
  (`AGENTS.md` / `CLAUDE.md` / `config.instructions`) and the compression-time
  `experimental.session.compacting`; there is no runtime dynamic message
  injection interface.
- **Present in the official type definitions** (the `Hooks` type in
  `@opencode-ai/plugin`); the source triggers it at `session/prompt.ts:1255`,
  and the `experimental.` prefix signals instability.
- **Alternative comparison:**
  - `experimental.chat.system.transform` — works, but injecting into the
    system prompt breaks prefix caching (system is first in the request;
    dynamic content forces a full recompute), and it is equally undocumented.
  - `chat.message` — no `experimental` prefix, but it fires **before** the
    message is saved; a splice injection would be persisted into history →
    pollutes ingest, so it cannot be used for recall.
  - Static `instructions` / `AGENTS.md` — officially supported but cannot
    change dynamically with the conversation.
- **The platform has no official memory backend**: opencode has no built-in
  memory/knowledge interface (relevant `/docs/memory|knowledge|context` all
  404); long-term memory relies on community plugins (e.g. opencode-mem) or
  self-built bridges like this one.
- **Conclusion**: keep the current `messages.transform` + in-place splice
  approach. It is undocumented, but it is the only interface satisfying all
  three constraints — "per-turn dynamic injection + no history pollution +
  prefix-cache friendly". If an opencode upgrade changes or removes this hook,
  recall injection will silently no-op (failure isolation guarantees sessions
  are never blocked); migrate per the release notes at that point.

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
        "config": {
          "agent": "openclaw"      // per-host namespace (default "default")
        },
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
  prompt text and reset on `before_agent_finalize` (the event carries no
  session id).
- Transcript ingest: `before_agent_finalize` caches the complete conversation
  per session id (the only event with both a stable `sessionId` and full
  `messages`); `session_end` pushes it once per session with that stable id,
  so multi-turn conversations produce a single session transcript (no
  per-run session fragmentation).
- Tools are named **`interest_search`** / **`interest_logs`** (not
  `memory_*`) so they never collide with the bundled `memory-core` plugin.
- Optional plugin config (`plugins.entries["interest-memory"].config`):
  `baseUrl`, `agent`, `timeoutMs` (overrides env). Priority is
  **plugin config > env** (`INTEREST_BASE_URL` / `INTEREST_AGENT` /
  `INTEREST_TIMEOUT`); `timeoutMs` is in milliseconds, env `INTEREST_TIMEOUT`
  in seconds. `agent` defaults to `"default"` — set it to a per-host namespace
  (e.g. `"openclaw"`) or your memory reads/writes share the `default`
  namespace with any other bridge that left it unset.
- Note: `session_end` fires when a session is replaced/timed out/reset or the
  gateway stops, so transcripts are pushed at session close rather than per
  turn (same contract as the pi bridge's `session_shutdown`).

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
node --test bridge/mcp-server/lib.test.mjs
node --test bridge/claudecode/hooks/lib.test.mjs
node --test bridge/codex/hooks/lib.test.mjs
node --test bridge/reasonix/hooks/lib.test.mjs
```

Each `lib` module is importable without the agent runtime, so the pure logic
(transcript reduction, URL/payload shaping) is covered in isolation. End-to-end
service behavior is covered by `scripts/e2e.sh` (needs LLM keys).
