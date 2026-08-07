/**
 * interest-memory — pi extension.
 *
 * Bridges pi sessions to the local interest-memory REST service:
 *   - before_agent_start: injects `GET /api/v1/{agent}/recall` context into
 *     each user turn as a custom (hidden) message.
 *   - session_shutdown: pushes the full transcript to
 *     `POST /api/v1/{agent}/sessions`.
 *   - consumer tools: `memory_search` (`GET /search`) and `memory_logs`
 *     (`GET /logs`).
 *
 * Config via environment variables:
 *   INTEREST_BASE_URL  — service base URL (default: http://127.0.0.1:8899)
 *   INTEREST_AGENT     — agent namespace (default: "pi")
 *   INTEREST_TIMEOUT   — per-request timeout seconds (default: 8)
 *
 * Deploy: place this file at `~/.pi/agent/extensions/memory.ts` (global) or
 * `.pi/extensions/memory.ts` (project-local, requires project trust).
 */

import { Type } from "typebox"
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { extractTurns, ingest, lastUserText, memoryConfig, memoryLogs, memorySearch, recall } from "./lib.ts"

export default function (pi: ExtensionAPI) {
  const cfg = memoryConfig()
  // Per-session recall/push dedupe: sessionID -> {lastRecallKey, lastPushedKey}
  const states = new Map<string, { lastRecallKey: string; lastPushedKey: string }>()

  // ③ consumer tools.
  pi.registerTool({
    name: "memory_search",
    label: "Memory Search",
    description:
      "Search the interest-memory knowledge base and return full entries (body/claims/evidence) with their relationship edges. Pass 'query' for a semantic search, or 'id' to fetch one specific page/interest point by id (id wins when both are given).",
    parameters: Type.Object({
      query: Type.Optional(Type.String({ description: "Semantic search query (topic, decision, or phrase)" })),
      id: Type.Optional(Type.String({ description: "Exact id of a wiki page or interest point to fetch" })),
      top_k: Type.Optional(Type.Integer({ minimum: 1, description: "Max results for query search (default 3)" })),
    }),
    async execute(_toolCallId, params) {
      const text = await memorySearch(cfg, params as { query?: string; id?: string; top_k?: number })
      return { content: [{ type: "text", text }], details: {} }
    },
  })

  pi.registerTool({
    name: "memory_logs",
    label: "Memory Logs",
    description:
      "Query the change-log of the interest-memory knowledge base: recent structural changes (page/interest-point title, action, and edges touched), newest first.",
    parameters: Type.Object({
      limit: Type.Optional(Type.Integer({ minimum: 0, description: "Max log entries (default 10)" })),
      offset: Type.Optional(Type.Integer({ minimum: 0, description: "Pagination offset (default 0)" })),
    }),
    async execute(_toolCallId, params) {
      const text = await memoryLogs(cfg, params as { limit?: number; offset?: number })
      return { content: [{ type: "text", text }], details: {} }
    },
  })

  // ① session-start recall injection (once per user turn).
  pi.on("before_agent_start", async (event, ctx) => {
    const query = event.prompt?.trim()
    if (!query) return
    const sessionID = ctx.sessionManager.getSessionId()
    if (!sessionID) return
    const state = states.get(sessionID) ?? { lastRecallKey: "", lastPushedKey: "" }
    states.set(sessionID, state)
    const key = `${query.slice(0, 200)}`
    if (key === state.lastRecallKey) return
    state.lastRecallKey = key
    const ctxText = await recall(cfg, query)
    if (!ctxText) return
    return {
      message: {
        customType: "interest-memory-recall",
        content: `<memory_context>\n${ctxText}\n</memory_context>`,
        display: false,
      },
    }
  })

  // ② session-end transcript push. getEntries() is complete here (the aborted
  // turn is settled before session_shutdown fires).
  pi.on("session_shutdown", async (_event, ctx) => {
    try {
      const sessionID = ctx.sessionManager.getSessionId()
      const entries = ctx.sessionManager.getEntries() as Array<{
        type?: string
        message?: { role?: string; content?: unknown; toolName?: string }
        role?: string
        content?: unknown
        toolName?: string
      }>
      const turns = extractTurns(entries)
      if (!turns.length) return
      if (!sessionID) return
      const state = states.get(sessionID) ?? { lastRecallKey: "", lastPushedKey: "" }
      states.set(sessionID, state)
      const lastKey = `${turns.length}:${turns[turns.length - 1].content.slice(0, 200)}`
      if (lastKey === state.lastPushedKey) return
      state.lastPushedKey = lastKey
      await ingest(cfg, sessionID, turns, new Date().toISOString())
    } catch (err) {
      // Failure isolation: never crash shutdown for a memory push.
      console.error(`[interest-memory] session_shutdown ingest failed: ${err instanceof Error ? err.message : String(err)}`)
    }
  })
}
