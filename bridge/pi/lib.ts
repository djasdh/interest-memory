/**
 * interest-memory — shared pure logic for the pi extension.
 *
 * Dependency-free module (only global `fetch`/`AbortSignal`), so tests can
 * import it without the pi runtime.
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
    agent: env.INTEREST_AGENT || "pi",
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
 * Extract `[{role, content}]` wire turns from pi SessionManager entries.
 * Accepts either the full entries array (filtered to `type === "message"`)
 * or a raw array of AgentMessage-like objects.
 *
 * Message shapes (pi AI types):
 *   - user:      { role:"user", content: string | TextContent[] }
 *   - assistant: { role:"assistant", content: TextContent[] | ToolCall[] | ThinkingContent[] }
 *   - toolResult:{ role:"toolResult", toolName, content: TextContent[], isError }
 */
export function extractTurns(
  entries: Array<{
    type?: string
    message?: AgentLike
    role?: string
    content?: unknown
    toolName?: string
  }>,
): WireTurn[] {
  const out: WireTurn[] = []
  for (const e of entries) {
    const msg = e.type === "message" ? e.message : e
    if (!msg) continue
    const role = msg.role
    if (role === "user" || role === "assistant") {
      const text = contentText(msg.content)
      if (text) out.push({ role, content: text })
    } else if (role === "toolResult") {
      const text = contentText(msg.content)
      if (text) out.push({ role: "tool_result", content: msg.toolName ? `${msg.toolName}: ${text}` : text })
    }
  }
  return out
}

interface AgentLike {
  role?: string
  content?: unknown
  toolName?: string
}

function contentText(content: unknown): string {
  if (content == null) return ""
  if (typeof content === "string") return content.trim()
  if (Array.isArray(content)) {
    return content
      .filter((c): c is { type: string; text: string } => c != null && typeof c === "object" && c.type === "text" && typeof c.text === "string")
      .map((c) => c.text)
      .join("\n")
      .trim()
  }
  return ""
}

/** Extract the last user text from entries (for the recall query). */
export function lastUserText(
  entries: Array<{ type?: string; message?: AgentLike; role?: string; content?: unknown }>,
): string {
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]
    const msg = e.type === "message" ? e.message : e
    if (msg?.role !== "user") continue
    const text = contentText(msg.content)
    if (text) return text
  }
  return ""
}

export function sessionStartedAt(): string {
  return new Date().toISOString()
}
