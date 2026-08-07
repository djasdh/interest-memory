/**
 * interest-memory — OpenClaw native plugin.
 *
 * Bridges OpenClaw conversations to the local interest-memory REST service:
 *   - before_prompt_build: injects `GET /api/v1/{agent}/recall` context.
 *   - agent_end: pushes the full transcript to `POST /api/v1/{agent}/sessions`.
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
  recall,
  resolveConfig,
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
    const cfg: InterestConfig = resolveConfig();
    const agent = cfg.agent;
    // Per-agent dedupe: {lastRecallKey} prevents repeated recall injection
    // within a turn (the event has no session id, so the prompt is the key,
    // reset on agent_end); {lastPushedKey} prevents re-ingesting the same
    // transcript tail.
    const states = new Map<string, { lastRecallKey: string; lastPushedKey: string }>();

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

    // ① session-start recall injection (once per user turn; deduped by prompt
    // text since the event carries no session id).
    api.on("before_prompt_build", async (event) => {
      const query = event.prompt?.trim();
      if (!query) return undefined;
      const st = states.get(agent) ?? { lastRecallKey: "", lastPushedKey: "" };
      const recallKey = query.slice(0, 200);
      if (recallKey === st.lastRecallKey) return undefined;
      st.lastRecallKey = recallKey;
      states.set(agent, st);
      const ctx = await recall(cfg.baseUrl, agent, query, cfg.timeoutMs);
      if (!ctx) return undefined;
      return { prependContext: `<memory_context>\n${ctx}\n</memory_context>` };
    });

    // ② session-end transcript ingest (complete messages are in the event).
    api.on("agent_end", async (event) => {
      try {
        const messages = (event.messages ?? []) as unknown[];
        const turns = extractTurns(messages as never);
        if (!turns.length) return;
        const sessionId = String(event.runId ?? "unknown");
        const st = states.get(agent) ?? { lastRecallKey: "", lastPushedKey: "" };
        const lastKey = `${turns.length}:${turns[turns.length - 1].content.slice(0, 200)}`;
        if (lastKey === st.lastPushedKey) return;
        st.lastPushedKey = lastKey;
        // Turn is over — allow a recall for the next user prompt.
        st.lastRecallKey = "";
        states.set(agent, st);
        await ingest(cfg.baseUrl, agent, sessionId, turns, new Date().toISOString());
      } catch (err) {
        api.logger.warn?.(`interest-memory: agent_end ingest failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    });

    // ②b session_end: only cursor cleanup (2s drain budget — no heavy IO).
    api.on("session_end", () => {
      // Dedupe state lives per agentId; nothing heavy to clean up here.
    });
  },
});
