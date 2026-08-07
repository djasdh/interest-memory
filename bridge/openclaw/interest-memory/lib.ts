/**
 * interest-memory — shared pure logic for the OpenClaw plugin.
 *
 * Dependency-free module (only global `fetch`/`AbortSignal`), so tests can
 * import it without the openclaw plugin runtime.
 */

export interface InterestConfig {
  baseUrl: string;
  timeoutMs: number;
  agent: string;
}

export function resolveConfig(env: Record<string, string | undefined> = process.env): InterestConfig {
  const raw = env.INTEREST_TIMEOUT;
  const t = raw ? Number(raw) : 8;
  return {
    baseUrl: (env.INTEREST_BASE_URL || "http://127.0.0.1:8899").replace(/\/+$/, ""),
    timeoutMs: (Number.isFinite(t) && t > 0 ? t : 8) * 1000,
    agent: env.INTEREST_AGENT || "default",
  };
}

export type AgentMessage = {
  role?: string;
  content?: string | Array<{ type?: string; text?: string }>;
  toolName?: string;
};

/**
 * Reduce OpenClaw AgentMessage[] to the wire format `[{role, content}]` the
 * backend transcript parser reads. role ∈ user|assistant|tool_result.
 */
export function extractTurns(messages: AgentMessage[]): Array<{ role: string; content: string }> {
  const out: Array<{ role: string; content: string }> = [];
  for (const m of messages) {
    if (m.role === "user" || m.role === "assistant") {
      const text = textOf(m.content);
      if (text) out.push({ role: m.role, content: text });
    } else if (m.role === "toolResult") {
      const text = textOf(m.content);
      if (text) out.push({ role: "tool_result", content: m.toolName ? `${m.toolName}: ${text}` : text });
    }
  }
  return out;
}

export function textOf(content: string | Array<{ type?: string; text?: string }> | undefined): string {
  if (!content) return "";
  if (typeof content === "string") return content.trim();
  return content
    .filter((c): c is { type?: string; text: string } => c.type === "text" && typeof c.text === "string")
    .map((c) => c.text)
    .join("\n")
    .trim();
}

export async function safeFetchText(url: URL, timeoutMs: number): Promise<string> {
  const res = await fetch(url, { signal: AbortSignal.timeout(timeoutMs) });
  if (!res.ok) {
    return JSON.stringify({ error: `status ${res.status}` });
  }
  return res.text();
}

export function parseMemoryContext(text: string): string {
  try {
    const payload = JSON.parse(text) as { memory_context?: string };
    return payload.memory_context ?? "";
  } catch {
    return "";
  }
}

/** POST /api/v1/{agent}/sessions with a transcript. Best-effort; never throws. */
export async function ingest(
  baseUrl: string,
  agent: string,
  sessionId: string,
  turns: Array<{ role: string; content: string }>,
  sessionDate?: string,
): Promise<boolean> {
  if (!turns.length) return false;
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(agent)}/sessions`, baseUrl);
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        session_id: sessionId,
        turn_count: turns.length,
        raw_turns: JSON.stringify(turns),
        session_date: sessionDate,
      }),
      signal: AbortSignal.timeout(15000),
    });
    return res.status === 200 || res.status === 201 || res.status === 202;
  } catch {
    return false;
  }
}

/** GET /api/v1/{agent}/recall?query= → bare text (or "" on failure). */
export async function recall(baseUrl: string, agent: string, query: string, timeoutMs: number): Promise<string> {
  if (!query) return "";
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(agent)}/recall`, baseUrl);
    url.searchParams.set("query", query);
    return parseMemoryContext(await safeFetchText(url, timeoutMs));
  } catch {
    return "";
  }
}

/** GET /api/v1/{agent}/search → items array JSON (or error JSON string). */
export async function memorySearch(
  baseUrl: string,
  agent: string,
  args: { query?: string; id?: string; top_k?: number },
  timeoutMs: number,
): Promise<string> {
  if (!args.query && !args.id) {
    return JSON.stringify({ error: "interest_search: missing 'query' or 'id'" });
  }
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(agent)}/search`, baseUrl);
    if (args.id) url.searchParams.set("id", args.id);
    else {
      url.searchParams.set("query", args.query ?? "");
      if (args.top_k && args.top_k > 0) url.searchParams.set("top_k", String(args.top_k));
    }
    const text = await safeFetchText(url, timeoutMs);
    try {
      const payload = JSON.parse(text) as { items?: unknown[] };
      return JSON.stringify(payload.items ?? []);
    } catch {
      return text;
    }
  } catch (err) {
    return JSON.stringify({ error: `interest_search: ${err instanceof Error ? err.message : String(err)}` });
  }
}

/** GET /api/v1/{agent}/logs → items array JSON (or error JSON string). */
export async function memoryLogs(
  baseUrl: string,
  agent: string,
  args: { limit?: number; offset?: number },
  timeoutMs: number,
): Promise<string> {
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(agent)}/logs`, baseUrl);
    const limit = args.limit !== undefined && Number.isFinite(args.limit) ? args.limit : 10;
    const offset = args.offset !== undefined && Number.isFinite(args.offset) ? args.offset : 0;
    url.searchParams.set("limit", String(Math.max(0, limit)));
    url.searchParams.set("offset", String(Math.max(0, offset)));
    const text = await safeFetchText(url, timeoutMs);
    try {
      const payload = JSON.parse(text) as { items?: unknown[] };
      return JSON.stringify(payload.items ?? []);
    } catch {
      return text;
    }
  } catch (err) {
    return JSON.stringify({ error: `interest_logs: ${err instanceof Error ? err.message : String(err)}` });
  }
}
