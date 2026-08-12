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
  buildMemoryTurn,
  cacheSnapshot,
  extractTurns,
  ingest,
  lastUserText,
  memoryConfig,
  memoryLogs,
  memorySearch,
  pushedKey,
  recall,
  setPushedKey,
  type WireTurn,
} from "./memory-lib.ts"

export default function MemoryPlugin(input: PluginInput): Hooks {
  const cfg = memoryConfig()
  // Per-session recall dedupe (in-memory only: recall should re-run on resume).
  const lastRecall = new Map<string, string>()
  // Latest full message list per session, captured from the transform hook.
  // Print/one-shot modes don't persist messages to the store, so this is the
  // only source for session-end ingest there.
  const lastTransformMessages = new Map<string, unknown[]>()

  // Hooks are assembled conditionally per INTEREST_MODE:
  //   input  → ingest only (no tools, no recall)
  //   output → recall + tools only (no ingest)
  const hooks: Hooks = { dispose: async () => {} }

  if (cfg.mode !== "input") {
    hooks.tool = {
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
    }
  }

  // Inject recall context once per user turn (before each LLM call) and
  // cache the latest message list for session-end ingest (print/one-shot
  // modes don't persist messages to the store, so client.session.messages
  // returns nothing there).
  if (cfg.mode !== "input") {
    hooks["experimental.chat.messages.transform"] = async (_input, output) => {
      const messages = output.messages as Array<{
        info?: { id?: string; sessionID?: string; role?: string }
        parts?: Array<{ type?: string; text?: string }>
      }>
      if (!messages.length) return
      const last = messages[messages.length - 1]
      const sessionID = last?.info?.sessionID
      // Snapshot BEFORE splicing: output.messages is the SAME array opencode
      // passes to toModelMessagesEffect, so the ingest cache must not include
      // the injected recall turn (else it'd be ingested back as a user turn).
      if (sessionID) lastTransformMessages.set(sessionID, cacheSnapshot(messages))
      const lastUser = lastUserText(messages)
      if (!lastUser) return
      if (!sessionID) return
      const key = `${last?.info?.id ?? ""}:${lastUser.slice(0, 200)}`
      if (key === lastRecall.get(sessionID)) return
      lastRecall.set(sessionID, key)
      const ctx = await recall(cfg, lastUser)
      if (!ctx) return
      // Clone the last real user message's info and swap in recall context so
      // the injected turn stays a well-formed UserMessage (toModelMessagesEffect
      // reads info.id/role and text parts). Insert right before the final
      // message so the model sees it as the incoming user turn. NOTE: must
      // mutate in place (push/splice) — the trigger contract is "mutate output
      // in place", and replacing output.messages is dropped by the caller.
      const baseInfo = messages
        .slice()
        .reverse()
        .find((m) => m.info?.role === "user")?.info
      const injected = buildMemoryTurn(baseInfo, ctx)
      output.messages.splice(messages.length - 1, 0, injected as never)
    }
  }

  // Push the transcript when the session goes idle or is deleted. Ingest is
  // deduped against the on-disk pushed fingerprint so a resumed session skips.
  if (cfg.mode !== "output") {
    hooks.event = async ({ event }) => {
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
        if (lastKey === pushedKey(cfg, sessionID)) return
        await ingest(cfg, sessionID, turns)
        setPushedKey(cfg, sessionID, lastKey)
        if (event.type === "session.deleted") lastRecall.delete(sessionID)
      } catch {
        // Failure isolation: never crash the session for a memory push.
      }
    }
  }

  return hooks
}
