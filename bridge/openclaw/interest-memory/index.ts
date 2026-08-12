/**
 * interest-memory — OpenClaw native plugin.
 *
 * Bridges OpenClaw conversations to the local interest-memory REST service:
 *   - before_prompt_build: injects `GET /api/v1/{agent}/recall` context
 *     (deduped by prompt text; reset on before_agent_finalize).
 *   - before_agent_finalize: caches the complete transcript per session id.
 *   - session_end: pushes the cached transcript once per session to
 *     `POST /api/v1/{agent}/sessions` with a stable session id.
 *   - consumer tools: `interest_search` (`GET /search`) and `interest_logs`
 *     (`GET /logs`). Named `interest_*` (not `memory_*`) so they never collide
 *     with the bundled memory-core plugin.
 *
 * Config via environment variables:
 *   INTEREST_BASE_URL  — service base URL (default: http://127.0.0.1:8899)
 *   INTEREST_AGENT     — agent namespace (default: "default")
 *   INTEREST_TIMEOUT   — per-request timeout seconds (default: 8)
 *
 * Deploy: copy this directory to `<configDir>/extensions/interest-memory/`
 * (default `~/.openclaw/extensions/interest-memory/`) and enable the plugin
 * in openclaw.json:
 *
 *   {
 *     "plugins": {
 *       "entries": {
 *         "interest-memory": {
 *           "enabled": true,
 *           "hooks": { "allowConversationAccess": true, "allowPromptInjection": true }
 *         }
 *       }
 *     }
 *   }
 */
import { buildJsonPluginConfigSchema, definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import { Type } from "typebox";
import {
  extractTurns,
  ingest,
  memoryLogs,
  memorySearch,
  pushedKey,
  recall,
  resolveConfig,
  setPushedKey,
  type InterestConfig,
} from "./lib.js";

export default definePluginEntry({
  id: "interest-memory",
  name: "Interest Memory",
  description:
    "interest-memory — session-start recall injection + session-end transcript ingest via a local Go memory service (interest_search / interest_logs tools).",
  configSchema: buildJsonPluginConfigSchema({
    type: "object",
    additionalProperties: false,
    properties: {
      baseUrl: { type: "string", description: "interest-memory service base URL (default: http://127.0.0.1:8899)" },
      agent: { type: "string", description: "agent namespace for /api/v1/{agent}/... (default: 'default')" },
      timeoutMs: { type: "integer", minimum: 1, description: "per-request timeout ms (default: 8000)" },
    },
  }),
  register(api) {
    const cfg: InterestConfig = resolveConfig(api.pluginConfig);
    const agent = cfg.agent;
    // recallKeys: per-agent dedupe of recall injection (the event has no
    // session id, so the prompt is the key; reset on before_agent_finalize).
    // transcripts: per-sessionId cache of the latest complete transcript
    // (from before_agent_finalize, which carries both sessionId and full
    // messages — unlike agent_end, which has no session id).
    const recallKeys = new Map<string, string>();
    const transcripts = new Map<string, Array<{ role: string; content: string }>>();

    // consumer tools (read side — omitted in input-only mode).
    if (cfg.mode !== "input") {
      api.registerTool(
        () => ({
          name: "interest_search",
          label: "Interest Memory Search",
          description:
            "Search the interest-memory knowledge base and return full entries (body/claims/evidence) with their relationship edges. Pass 'query' for a semantic search, or 'id' to fetch one specific page/interest point by id (id wins when both are given).",
          parameters: Type.Object({
            query: Type.String({ description: "Semantic search query (topic, decision, or phrase)" }),
            id: Type.Optional(Type.String({ description: "Exact id of a wiki page or interest point to fetch" })),
            top_k: Type.Optional(Type.Integer({ minimum: 1, description: "Max results for query search (default 3)" })),
          }),
          async execute(_toolCallId, params) {
            const { query, id, top_k } = params as { query?: string; id?: string; top_k?: number };
            const text = await memorySearch(cfg.baseUrl, agent, { query, id, top_k }, cfg.timeoutMs);
            return { content: [{ type: "text", text }], details: {} };
          },
        }),
        { name: "interest_search" },
      );

      api.registerTool(
        () => ({
          name: "interest_logs",
          label: "Interest Memory Logs",
          description:
            "Query the change-log of the interest-memory knowledge base: recent structural changes (page/interest-point title, action, and edges touched), newest first.",
          parameters: Type.Object({
            limit: Type.Optional(Type.Integer({ minimum: 0, description: "Max log entries (default 10)" })),
            offset: Type.Optional(Type.Integer({ minimum: 0, description: "Pagination offset (default 0)" })),
          }),
          async execute(_toolCallId, params) {
            const { limit, offset } = params as { limit?: number; offset?: number };
            const text = await memoryLogs(cfg.baseUrl, agent, { limit, offset }, cfg.timeoutMs);
            return { content: [{ type: "text", text }], details: {} };
          },
        }),
        { name: "interest_logs" },
      );
    }

    // ① session-start recall injection (read side — omitted in input-only
    // mode; deduped by prompt text since the event carries no session id).
    if (cfg.mode !== "input") {
      api.on("before_prompt_build", async (event) => {
        const query = event.prompt?.trim();
        if (!query) return undefined;
        const recallKey = query.slice(0, 200);
        if (recallKey === recallKeys.get(agent)) return undefined;
        recallKeys.set(agent, recallKey);
        const ctx = await recall(cfg.baseUrl, agent, query, cfg.timeoutMs);
        if (!ctx) return undefined;
        return { prependContext: `<memory_context>\n${ctx}\n</memory_context>` };
      });
    }

    // ② cache the complete transcript per session. Runs on run finalization
    // (before the terminal delivery), so event.messages holds the full
    // conversation and event.sessionId is stable across turns.
    api.on("before_agent_finalize", (event) => {
      try {
        const messages = event.messages;
        if (!Array.isArray(messages) || !messages.length) return;
        const turns = extractTurns(messages as never);
        if (!turns.length) return;
        // Turn is over — allow a recall for the next user prompt.
        recallKeys.set(agent, "");
        transcripts.set(event.sessionId, turns);
      } catch (err) {
        api.logger.warn?.(`interest-memory: before_agent_finalize cache failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    });

    // ②b session_end: push the cached transcript once per session (write side
    // — omitted in output-only mode), deduped against the on-disk pushed
    // fingerprint so a resumed session skips re-ingest.
    if (cfg.mode !== "output") {
      api.on("session_end", async (event) => {
        try {
          const turns = transcripts.get(event.sessionId);
          if (!turns?.length) return;
          const lastKey = `${turns.length}:${turns[turns.length - 1].content.slice(0, 200)}`;
          if (lastKey === pushedKey(agent, event.sessionId)) return;
          await ingest(cfg.baseUrl, agent, event.sessionId, turns, new Date().toISOString());
          setPushedKey(agent, event.sessionId, lastKey);
          transcripts.delete(event.sessionId);
        } catch (err) {
          api.logger.warn?.(`interest-memory: session_end ingest failed: ${err instanceof Error ? err.message : String(err)}`);
        }
      });
    }
  },
});
