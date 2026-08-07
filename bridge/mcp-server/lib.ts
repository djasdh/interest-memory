/**
 * interest-memory MCP server — shared pure logic.
 *
 * Dependency-free module (only global `fetch`/`AbortSignal`), so tests can
 * import it without the MCP SDK runtime.
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
    agent: env.INTEREST_AGENT || "default",
    timeoutMs: (Number.isFinite(t) && t > 0 ? t : DEFAULT_TIMEOUT) * 1000,
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
