/**
 * interest-memory bridge — shared pure logic for the opencode plugin.
 *
 * Dependency-free module (only global `fetch`/`AbortSignal`), so tests can
 * import it without the opencode plugin runtime.
 */

const DEFAULT_BASE_URL = "http://127.0.0.1:8899"
const DEFAULT_TIMEOUT = 8.0

export interface MemoryConfig {
  baseUrl: string
  agent: string
  timeoutMs: number
}

export function memoryConfig(env: Record<string, string | undefined> = process.env): MemoryConfig {
  const base = (env.INTEREST_BASE_URL || DEFAULT_BASE_URL).replace(/\/+$/, "")
  const raw = env.INTEREST_TIMEOUT
  const t = raw ? Number(raw) : DEFAULT_TIMEOUT
  return {
    baseUrl: base,
    agent: env.INTEREST_AGENT || "opencode",
    timeoutMs: (Number.isFinite(t) && t > 0 ? t : DEFAULT_TIMEOUT) * 1000,
  }
}

export interface WireTurn {
  role: string
  content: string
}

/** GET /api/v1/{agent}/recall?query= → bare text (or "" on failure). */
export async function recall(cfg: MemoryConfig, query: string): Promise<string> {
  if (!query) return ""
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(cfg.agent)}/recall`, cfg.baseUrl)
    url.searchParams.set("query", query)
    const res = await fetch(url, { signal: AbortSignal.timeout(cfg.timeoutMs) })
    if (res.status !== 200) return ""
    const payload = (await res.json()) as { memory_context?: string }
    return payload.memory_context ?? ""
  } catch {
    return ""
  }
}

/** POST /api/v1/{agent}/sessions with a transcript. Best-effort; never throws. */
export async function ingest(
  cfg: MemoryConfig,
  sessionID: string,
  turns: WireTurn[],
  sessionDate?: string,
): Promise<boolean> {
  if (!turns.length) return false
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(cfg.agent)}/sessions`, cfg.baseUrl)
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        session_id: sessionID,
        turn_count: turns.length,
        raw_turns: JSON.stringify(turns),
        session_date: sessionDate,
      }),
      signal: AbortSignal.timeout(Math.max(cfg.timeoutMs, 15000)),
    })
    return res.status === 200 || res.status === 201 || res.status === 202
  } catch {
    return false
  }
}

/** GET /api/v1/{agent}/search → items array JSON (or error JSON string). */
export async function memorySearch(
  cfg: MemoryConfig,
  args: { query?: string; id?: string; top_k?: number },
): Promise<string> {
  if (!args.query && !args.id) {
    return JSON.stringify({ error: "memory_search: missing 'query' or 'id'" })
  }
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(cfg.agent)}/search`, cfg.baseUrl)
    if (args.id) url.searchParams.set("id", args.id)
    else {
      url.searchParams.set("query", args.query!)
      if (args.top_k && args.top_k > 0) url.searchParams.set("top_k", String(args.top_k))
    }
    const res = await fetch(url, { signal: AbortSignal.timeout(cfg.timeoutMs) })
    if (res.status !== 200) return JSON.stringify({ error: `memory_search: status ${res.status}` })
    const payload = (await res.json()) as { items?: unknown[] }
    return JSON.stringify(payload.items ?? [])
  } catch (err) {
    return JSON.stringify({ error: `memory_search: ${err instanceof Error ? err.message : String(err)}` })
  }
}

/** GET /api/v1/{agent}/logs → items array JSON (or error JSON string). */
export async function memoryLogs(
  cfg: MemoryConfig,
  args: { limit?: number; offset?: number },
): Promise<string> {
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(cfg.agent)}/logs`, cfg.baseUrl)
    const limit = args.limit !== undefined && Number.isFinite(args.limit) ? args.limit : 10
    const offset = args.offset !== undefined && Number.isFinite(args.offset) ? args.offset : 0
    url.searchParams.set("limit", String(Math.max(0, limit)))
    url.searchParams.set("offset", String(Math.max(0, offset)))
    const res = await fetch(url, { signal: AbortSignal.timeout(cfg.timeoutMs) })
    if (res.status !== 200) return JSON.stringify({ error: `memory_logs: status ${res.status}` })
    const payload = (await res.json()) as { items?: unknown[] }
    return JSON.stringify(payload.items ?? [])
  } catch (err) {
    return JSON.stringify({ error: `memory_logs: ${err instanceof Error ? err.message : String(err)}` })
  }
}

/**
 * Reduce opencode `{info, parts}[]` messages to the wire format
 * `[{role, content}]` the backend transcript parser reads.
 * - role: only user/assistant kept (system-like roles don't exist here).
 * - content: joined text parts of the message; tool parts are emitted as
 *   separate `tool_result` turns so the backend sees tool context.
 */
export function extractTurns(
  messages: Array<{
    info?: { role?: string; id?: string }
    parts?: Array<{
      type?: string
      text?: string
      tool?: string
      state?: { status?: string; output?: string; error?: string }
    }>
  }>,
): WireTurn[] {
  if (!Array.isArray(messages)) return []
  const out: WireTurn[] = []
  for (const m of messages) {
    const role = m.info?.role
    if (role !== "user" && role !== "assistant") continue
    const textParts = (m.parts ?? [])
      .filter((p) => p.type === "text" && typeof p.text === "string")
      .map((p) => p.text as string)
      .join("\n")
      .trim()
    if (textParts) out.push({ role, content: textParts })
    for (const p of m.parts ?? []) {
      if (p.type !== "tool") continue
      const st = p.state
      const output =
        st?.status === "completed" ? st.output : st?.status === "error" ? st.error : undefined
      const text = output?.trim()
      if (text) out.push({ role: "tool_result", content: `${p.tool ?? "tool"}: ${text}` })
    }
  }
  return out
}

/**
 * Build the injected recall turn: a UserMessage whose info is cloned from the
 * last real user message (so `toModelMessagesEffect` keeps a well-formed
 * UserMessage) and whose text parts carry the `<memory_context>` block.
 */
export function buildMemoryTurn(
  baseInfo: Record<string, unknown> | undefined,
  ctx: string,
): { info: Record<string, unknown>; parts: Array<{ type: string; text: string }> } {
  return {
    info: { ...(baseInfo ?? {}), id: `memory-recall-${Date.now()}` },
    parts: [{ type: "text", text: `<memory_context>\n${ctx}\n</memory_context>` }],
  }
}

/**
 * Snapshot a message list for session-end ingest. The transform hook splices
 * the recall turn into the SAME array opencode passes us (`output.messages` is
 * a reference to the session message list), so the ingest cache must be a copy
 * taken BEFORE the splice — otherwise the injected turn would be ingested back
 * into the memory store as a real user turn (recall-loop pollution).
 */
export function cacheSnapshot<T>(messages: T[]): T[] {
  return messages.slice()
}

/** Extract the last user text from a message list (for the recall query). */
export function lastUserText(
  messages: Array<{
    info?: { role?: string }
    parts?: Array<{ type?: string; text?: string }>
  }>,
): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i]
    if (m.info?.role !== "user") continue
    const text = (m.parts ?? [])
      .filter((p) => p.type === "text" && typeof p.text === "string")
      .map((p) => p.text as string)
      .join("\n")
      .trim()
    if (text) return text
  }
  return ""
}
