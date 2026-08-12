/**
 * interest-memory — shared pure logic for the OpenClaw plugin.
 *
 * Dependency-free module (only global `fetch`/`AbortSignal` plus node
 * builtins), so tests can import it without the openclaw plugin runtime.
 */

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { homedir } from "node:os";

const MAX_STATE_SESSIONS = 10;

export type InterestMode = "auto" | "input" | "output";

export interface InterestConfig {
  baseUrl: string;
  timeoutMs: number;
  agent: string;
  mode: InterestMode;
}

export function resolveConfig(
  pluginConfig?: Record<string, unknown>,
  env: Record<string, string | undefined> = process.env,
): InterestConfig {
  const rawTimeout = env.INTEREST_TIMEOUT;
  const t = rawTimeout ? Number(rawTimeout) : 8;
  const p = (pluginConfig ?? {}) as Record<string, unknown>;
  const cfgAgent = typeof p.agent === "string" && p.agent.trim() ? p.agent : "";
  const cfgBase = typeof p.baseUrl === "string" && p.baseUrl.trim() ? p.baseUrl : "";
  // pluginConfig.timeoutMs is already in milliseconds (config schema says "timeout ms").
  const cfgTimeout = typeof p.timeoutMs === "number" && Number.isFinite(p.timeoutMs) && p.timeoutMs > 0 ? p.timeoutMs : 0;
  return {
    baseUrl: (cfgBase || env.INTEREST_BASE_URL || "http://127.0.0.1:8899").replace(/\/+$/, ""),
    timeoutMs: cfgTimeout || (Number.isFinite(t) && t > 0 ? t : 8) * 1000,
    agent: cfgAgent || env.INTEREST_AGENT || "default",
    mode: normalizeMode(env.INTEREST_MODE),
  };
}

function normalizeMode(v?: string): InterestMode {
  if (v === "input" || v === "output") return v;
  return "auto";
}

// ── durable per-session ingest dedupe (resume protection) ────────────────
//
// Keeps the last pushed transcript fingerprint per session on disk so a
// resumed session (process restart, same session id) skips re-ingesting an
// unchanged transcript. Only the 10 most recent sessions are retained.

function statePath(): string {
  return process.env.INTEREST_STATE_FILE || join(homedir(), ".interest-memory", "bridge-state.json");
}

function loadState(): Record<string, Record<string, string>> {
  try {
    const parsed = JSON.parse(readFileSync(statePath(), "utf8"));
    return parsed && typeof parsed === "object" ? (parsed as Record<string, Record<string, string>>) : {};
  } catch {
    return {};
  }
}

function saveState(state: Record<string, Record<string, string>>): void {
  try {
    mkdirSync(dirname(statePath()), { recursive: true });
    writeFileSync(statePath(), JSON.stringify(state));
  } catch {
    // best-effort: dedupe is an optimization, not a correctness guarantee
  }
}

/** Last pushed fingerprint for a session ("" if never pushed). */
export function pushedKey(agent: string, sessionID: string): string {
  return loadState()[agent]?.[sessionID] ?? "";
}

/** Record a session's last pushed fingerprint, retaining the 10 most recent. */
export function setPushedKey(agent: string, sessionID: string, key: string): void {
  const state = loadState();
  const agentMap = state[agent] ?? {};
  delete agentMap[sessionID]; // move to end = most recent
  agentMap[sessionID] = key;
  const keys = Object.keys(agentMap);
  while (keys.length > MAX_STATE_SESSIONS) {
    const oldest = keys.shift();
    if (oldest) delete agentMap[oldest];
  }
  state[agent] = agentMap;
  saveState(state);
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
