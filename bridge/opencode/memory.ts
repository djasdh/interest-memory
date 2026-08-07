/**
 * interest-memory bridge — opencode plugin.
 *
 * Connects opencode sessions to the local interest-memory REST service:
 *   - session-start recall: injects `GET /api/v1/{agent}/recall` context into
 *     the model prompt once per user turn.
 *   - session-end ingest: pushes the full transcript to
 *     `POST /api/v1/{agent}/sessions` when the session goes idle.
 *   - consumer tools: `memory_search` (`GET /search`) and `memory_logs`
 *     (`GET /logs`) registered as custom tools.
 *
 * Config via environment variables:
 *   INTEREST_BASE_URL  — service base URL (default: http://127.0.0.1:8899)
 *   INTEREST_AGENT     — agent namespace (default: "opencode")
 *   INTEREST_TIMEOUT   — per-request timeout seconds (default: 8)
 *
 * Deploy by placing this file at `~/.config/opencode/plugin/memory.ts`
 * (global) or `<project>/.opencode/plugin/memory.ts` (project-local).
 * Local plugins must export a default object with an `id` and `server()`.
 */

import { tool, type Hooks, type PluginInput } from "@opencode-ai/plugin"
import {
  extractTurns,
  ingest,
  lastUserText,
  memoryConfig,
  memoryLogs,
  memorySearch,
  recall,
  type WireTurn,
} from "./memory-lib.ts"

/** In-memory per-session state: dedupe recall + track pushed message ids. */
interface SessionState {
  lastRecallKey: string
  lastPushedKey: string
}

export default function MemoryPlugin(input: PluginInput): Hooks {
  const cfg = memoryConfig()
  const states = new Map<string, SessionState>()
  // Latest full message list per session, captured from the transform hook.
  // Print/one-shot modes don't persist messages to the store, so this is the
  // only source for session-end ingest there.
  const lastTransformMessages = new Map<string, unknown[]>()

  return {
    tool: {
      memory_search: tool({
        description:
          "Search the interest-memory knowledge base and return full entries (body/claims/evidence) with their relationship edges. Pass 'query' for a semantic search, or 'id' to fetch one specific page/interest point by id (id wins when both are given).",
        args: {
          query: tool.schema.string().optional().describe("Semantic search query (topic, decision, or phrase)"),
          id: tool.schema.string().optional().describe("Exact id of a wiki page or interest point to fetch"),
          top_k: tool.schema.number().optional().describe("Max results for query search (default 3)"),
        },
        async execute(args) {
          return { output: await memorySearch(cfg, args) }
        },
      }),
      memory_logs: tool({
        description:
          "Query the change-log of the interest-memory knowledge base: recent structural changes (page/interest-point title, action, and edges touched), newest first.",
        args: {
          limit: tool.schema.number().optional().describe("Max log entries (default 10)"),
          offset: tool.schema.number().optional().describe("Pagination offset (default 0)"),
        },
        async execute(args) {
          return { output: await memoryLogs(cfg, args) }
        },
      }),
    },

    // Inject recall context once per user turn (before each LLM call) and
    // cache the latest message list for session-end ingest (print/one-shot
    // modes don't persist messages to the store, so client.session.messages
    // returns nothing there).
    "experimental.chat.messages.transform": async (_input, output) => {
      const messages = output.messages as Array<{
        info?: { id?: string; sessionID?: string; role?: string }
        parts?: Array<{ type?: string; text?: string }>
      }>
      if (!messages.length) return
      const last = messages[messages.length - 1]
      const sessionID = last?.info?.sessionID
      if (sessionID) lastTransformMessages.set(sessionID, messages)
      const lastUser = lastUserText(messages)
      if (!lastUser) return
      if (!sessionID) return
      const state = states.get(sessionID) ?? { lastRecallKey: "", lastPushedKey: "" }
      states.set(sessionID, state)
      const key = `${last?.info?.id ?? ""}:${lastUser.slice(0, 200)}`
      if (key === state.lastRecallKey) return
      state.lastRecallKey = key
      const ctx = await recall(cfg, lastUser)
      if (!ctx) return
      // Clone the last real user message's info and swap in recall context so
      // the injected turn stays a well-formed UserMessage (toModelMessagesEffect
      // reads info.id/role and text parts). Insert right before the final
      // message so the model sees it as the incoming user turn.
      const baseInfo = messages
        .slice()
        .reverse()
        .find((m) => m.info?.role === "user")?.info
      output.messages = [
        ...messages.slice(0, -1),
        {
          info: { ...baseInfo, id: `memory-recall-${Date.now()}` },
          parts: [{ type: "text", text: `<memory_context>\n${ctx}\n</memory_context>` }],
        },
        messages[messages.length - 1],
      ] as never
    },

    // Push the transcript when the session goes idle or is deleted.
    event: async ({ event }) => {
      if (event.type !== "session.status" && event.type !== "session.deleted") return
      const properties = event.properties as {
        sessionID?: string
        status?: { type?: string }
        info?: { id?: string }
      }
      const sessionID = properties?.sessionID || properties?.info?.id
      if (!sessionID) return
      const isIdle = event.type === "session.deleted" || properties?.status?.type === "idle"
      if (!isIdle) return

      const state = states.get(sessionID) ?? { lastRecallKey: "", lastPushedKey: "" }
      states.set(sessionID, state)

      // Give in-flight parts a moment to flush before pulling the transcript.
      await new Promise((r) => setTimeout(r, 2000))

      try {
        let raw: unknown = lastTransformMessages.get(sessionID)
        // In interactive/TUI sessions messages are persisted; prefer the store
        // (complete transcript). Fall back to the transform-cached list.
        try {
          const stored = await input.client.session.messages({ path: { id: sessionID } })
          const data = (stored as { data?: unknown }).data
          if (Array.isArray(data) && data.length) raw = data
        } catch {
          // store unavailable; fall through to cache
        }
        const messages = Array.isArray(raw) ? (raw as never) : []
        const turns = extractTurns(messages)
        if (!turns.length) return
        const lastKey = `${turns.length}:${turns[turns.length - 1].content.slice(0, 200)}`
        if (lastKey === state.lastPushedKey) return
        state.lastPushedKey = lastKey
        await ingest(cfg, sessionID, turns)
        if (event.type === "session.deleted") states.delete(sessionID)
      } catch {
        // Failure isolation: never crash the session for a memory push.
      }
    },

    dispose: async () => {
      // Best-effort: nothing buffered outside the event handler.
    },
  }
}
