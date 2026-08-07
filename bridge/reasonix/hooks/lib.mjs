/**
 * interest-memory bridge — shared logic for Reasonix hooks.
 *
 * Dependency-free (node:fs + global fetch), so tests can import it directly.
 */
import { readFileSync } from "node:fs"

const DEFAULT_BASE_URL = "http://127.0.0.1:8899"
const DEFAULT_TIMEOUT = 8.0

export function memoryConfig(env = process.env) {
  const base = (env.INTEREST_BASE_URL || DEFAULT_BASE_URL).replace(/\/+$/, "")
  const raw = env.INTEREST_TIMEOUT
  const t = raw ? Number(raw) : DEFAULT_TIMEOUT
  return {
    baseUrl: base,
    agent: env.INTEREST_AGENT || "reasonix",
    timeoutMs: (Number.isFinite(t) && t > 0 ? t : DEFAULT_TIMEOUT) * 1000,
  }
}

/** GET /api/v1/{agent}/recall?query= → bare text (or "" on failure). */
export async function recall(cfg, query) {
  if (!query) return ""
  try {
    const url = new URL(`/api/v1/${encodeURIComponent(cfg.agent)}/recall`, cfg.baseUrl)
    url.searchParams.set("query", query)
    const res = await fetch(url, { signal: AbortSignal.timeout(cfg.timeoutMs) })
    if (res.status !== 200) return ""
    const payload = await res.json()
    return payload.memory_context ?? ""
  } catch {
    return ""
  }
}

/** POST /api/v1/{agent}/sessions with a transcript. Best-effort; never throws. */
export async function ingest(cfg, sessionID, turns, sessionDate) {
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

/**
 * Reduce a Reasonix session jsonl (`{role, content}` per line, OpenAI chat
 * format) to `[{role, content}]` wire turns. The wire format only wants
 * user/assistant; system and tool messages are dropped.
 */
export function parseReasonixTranscript(path) {
  const out = []
  const text = readFileSafe(path)
  if (!text) return out
  for (const line of text.split("\n")) {
    const trimmed = line.trim()
    if (!trimmed) continue
    let obj
    try {
      obj = JSON.parse(trimmed)
    } catch {
      continue
    }
    const role = obj.role
    if (role !== "user" && role !== "assistant") continue
    const content = contentText(obj.content)
    if (content) out.push({ role, content })
  }
  return out
}

function readFileSafe(path) {
  try {
    return readFileSync(path, "utf8")
  } catch {
    return ""
  }
}

function contentText(content) {
  if (content == null) return ""
  if (typeof content === "string") return content.trim()
  if (Array.isArray(content)) {
    return content
      .filter((c) => c != null && typeof c === "object" && c.type === "text" && typeof c.text === "string")
      .map((c) => c.text)
      .join("\n")
      .trim()
  }
  return ""
}
