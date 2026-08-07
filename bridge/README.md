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

**API 稳定性说明（2026-08 审计）**

`experimental.chat.messages.transform` 是当前**唯一**能做每轮动态 recall
注入的 opencode 接口，但需要注意它的官方地位：

- **未列入官方文档**。opencode 文档（`/docs/plugins/` 事件列表、
  `/docs/rules/`、`/docs/config/`）承诺的注入机制只有静态指令文件
  （`AGENTS.md` / `CLAUDE.md` / `config.instructions`），以及压缩时触发的
  `experimental.session.compacting`；没有运行时动态消息注入接口。
- **存在于官方类型定义**（`@opencode-ai/plugin` 的 `Hooks` 类型），
  源码在 `session/prompt.ts:1255` 触发，`experimental.` 前缀即不稳定信号。
- **替代项对比**：
  - `experimental.chat.system.transform` — 生效但注入 system prompt 会破坏
    前缀缓存（system 在请求最前，动态内容导致整段重算），且同样未文档化。
  - `chat.message` — 无 `experimental` 前缀，但触发于消息**保存前**，
    splice 注入会持久化进历史 → 污染 ingest，不能用于 recall。
  - 静态 `instructions`/`AGENTS.md` — 官方承诺但无法随对话动态变化。
- **平台无官方记忆后端**：opencode 无内置 memory/knowledge 接口（相关
  `/docs/memory|knowledge|context` 均 404），长期记忆靠社区插件
  （如 opencode-mem）或本项目这类自建桥接。
- **结论**：保持当前 `messages.transform` + 原地 splice 写法。它虽未文档化，
  但是唯一满足"每轮动态注入 + 不污染历史 + 前缀缓存友好"三条件的接口。
  升级 opencode 后如该 hook 变更/移除，recall 注入会静默失效（失败隔离
  已保证不阻断会话），届时按发行说明迁移。

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
