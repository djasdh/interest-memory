/**
 * interest-memory bridge — shared logic for Codex hooks.
 *
 * Dependency-free (node:fs + global fetch), so tests can import it directly.
 */
import { mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { homedir } from "node:os"

const DEFAULT_BASE_URL = "http://127.0.0.1:8899"
const DEFAULT_TIMEOUT = 8.0
const MAX_STATE_SESSIONS = 10

export function memoryConfig(env = process.env) {
  const base = (env.INTEREST_BASE_URL || DEFAULT_BASE_URL).replace(/\/+$/, "")
  const raw = env.INTEREST_TIMEOUT
  const t = raw ? Number(raw) : DEFAULT_TIMEOUT
  return {
    baseUrl: base,
    agent: env.INTEREST_AGENT || "codex",
    timeoutMs: (Number.isFinite(t) && t > 0 ? t : DEFAULT_TIMEOUT) * 1000,
    mode: normalizeMode(env.INTEREST_MODE),
  }
}

function normalizeMode(v) {
  if (v === "input" || v === "output") return v
  return "auto"
}

// ── durable per-session ingest dedupe (resume protection) ────────────────

function statePath() {
  return process.env.INTEREST_STATE_FILE || join(homedir(), ".interest-memory", "bridge-state.json")
}

function loadState() {
  try {
    const parsed = JSON.parse(readFileSync(statePath(), "utf8"))
    return parsed && typeof parsed === "object" ? parsed : {}
  } catch {
    return {}
  }
}

function saveState(state) {
  try {
    mkdirSync(dirname(statePath()), { recursive: true })
    writeFileSync(statePath(), JSON.stringify(state))
  } catch {
    // best-effort
  }
}

/** Last pushed fingerprint for a session ("" if never pushed). */
export function pushedKey(agent, sessionID) {
  return loadState()[agent]?.[sessionID] ?? ""
}

/** Record a session's last pushed fingerprint, retaining the 10 most recent. */
export function setPushedKey(agent, sessionID, key) {
  const state = loadState()
  const agentMap = state[agent] ?? {}
  delete agentMap[sessionID] // move to end = most recent
  agentMap[sessionID] = key
  const keys = Object.keys(agentMap)
  while (keys.length > MAX_STATE_SESSIONS) {
    const oldest = keys.shift()
    if (oldest) delete agentMap[oldest]
  }
  state[agent] = agentMap
  saveState(state)
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
 * Reduce a Codex rollout jsonl to `[{role, content}]` wire turns.
 * Codex format: `{"type":"response_item","payload":{"type":"message","role":...,"content":[{"type":"input_text"|"output_text","text":...}]}}`.
 * User text lives in `input_text`, assistant text in `output_text`; the
 * developer/system role is skipped.
 */
export function parseCodexTranscript(path) {
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
    if (obj.type !== "response_item") continue
    const payload = obj.payload
    if (!payload || payload.type !== "message") continue
    const role = payload.role
    if (role !== "user" && role !== "assistant") continue
    const content = payload.content
    if (!Array.isArray(content)) continue
    const parts = content
      .filter((c) => c != null && typeof c === "object" && c.type === (role === "assistant" ? "output_text" : "input_text") && typeof c.text === "string")
      .map((c) => c.text)
    const joined = parts.join("\n").trim()
    if (joined) out.push({ role, content: joined })
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
