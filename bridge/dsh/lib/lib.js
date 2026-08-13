/**
 * Pure helpers for the DSH bridge — dependency-free so `node --test` can
 * import them without the DSH runtime.
 *
 * Wire format matches the interest-memory REST API and the other bridges
 * (bridge/hermes, bridge/opencode, …): `[{ role, content }]` turns pushed to
 * `POST /api/v1/{agent}/sessions`; recall returned as a bare-text
 * `memory_context` from `GET /api/v1/{agent}/recall`.
 */

export const DEFAULT_CONFIG = {
  baseUrl: 'http://127.0.0.1:8899',
  agent: 'dsh',
  recallTimeoutMs: 5000,
  ingestTimeoutMs: 15000,
  traceDir: '',
}

/** Fill absent fields and normalize baseUrl (trailing slash stripped). */
export function normalizeConfig(input = {}) {
  return {
    baseUrl: String(input.baseUrl ?? DEFAULT_CONFIG.baseUrl).replace(/\/+$/, ''),
    agent: String(input.agent ?? DEFAULT_CONFIG.agent),
    recallTimeoutMs: Number(input.recallTimeoutMs ?? DEFAULT_CONFIG.recallTimeoutMs),
    ingestTimeoutMs: Number(input.ingestTimeoutMs ?? DEFAULT_CONFIG.ingestTimeoutMs),
    traceDir: String(input.traceDir ?? DEFAULT_CONFIG.traceDir),
  }
}

/**
 * Join the text of a ContentBlock[] (the model-visible text). Recurses into
 * `tool-result` blocks, whose text lives in their NESTED content array —
 * without this, tool results vanish from transcripts entirely.
 */
export function textBlocks(blocks) {
  if (!Array.isArray(blocks)) return ''
  const parts = []
  for (const b of blocks) {
    if (!b) continue
    if (b.type === 'text' && typeof b.text === 'string') parts.push(b.text)
    else if (b.type === 'tool-result' && Array.isArray(b.content)) parts.push(textBlocks(b.content))
  }
  return parts.join('\n').trim()
}

/**
 * Owned runtime-context snapshots (source.kind='plugin',
 * plugin='@deepseek-ai/dsh-system-prompt') are framework-generated user-role
 * context — never user speech. Ingest and query extraction must skip them, or
 * the injected memory would be ingested back (recall-loop pollution).
 */
export function isOwnedSnapshot(message) {
  return !!(
    message &&
    message.source &&
    message.source.kind === 'plugin' &&
    message.source.plugin === '@deepseek-ai/dsh-system-prompt'
  )
}

/**
 * Reduce derived DSH messages to the interest-memory wire format. Tool
 * results become their own `tool_result` turns so the backend extractor sees
 * tool context; owned snapshots are dropped; empty text is dropped.
 */
export function wireTurns(messages) {
  const out = []
  for (const m of messages || []) {
    if (isOwnedSnapshot(m)) continue
    const isTool = Array.isArray(m.content) && m.content.some((b) => b && b.type === 'tool-result')
    const text = textBlocks(m.content)
    if (!text) continue
    if (m.role === 'user') out.push({ role: isTool ? 'tool_result' : 'user', content: text })
    else if (m.role === 'assistant') out.push({ role: 'assistant', content: text })
  }
  return out
}

/** The last real user utterance (skips tool results and owned snapshots). */
export function lastUserQuery(messages) {
  for (let i = (messages || []).length - 1; i >= 0; i--) {
    const m = messages[i]
    if (!m || m.role !== 'user') continue
    if (isOwnedSnapshot(m)) continue
    if (Array.isArray(m.content) && m.content.some((b) => b && b.type === 'tool-result')) continue
    const text = textBlocks(m.content)
    if (text) return text
  }
  return ''
}

/**
 * Resume-protection fingerprint for one ingest: turn count + tail of the last
 * turn. In-memory per plugin instance; matches the intent of the other
 * bridges' durable `bridge-state.json` (kept per-process here because a DSH
 * session already restarts the plugin fresh).
 */
export function fingerprintOf(turns) {
  if (!Array.isArray(turns) || turns.length === 0) return ''
  const last = turns[turns.length - 1]
  return String(turns.length) + ':' + String((last && last.content) || '').slice(0, 200)
}

export function recallUrl(cfg, query) {
  return `/api/v1/${encodeURIComponent(cfg.agent)}/recall?query=${encodeURIComponent(query)}`
}

export function searchUrl(cfg, args) {
  const parts = []
  if (args.id) parts.push('id=' + encodeURIComponent(String(args.id)))
  else if (args.query) parts.push('query=' + encodeURIComponent(String(args.query)))
  if (args.top_k !== undefined) parts.push('top_k=' + encodeURIComponent(String(args.top_k)))
  return `/api/v1/${encodeURIComponent(cfg.agent)}/search?${parts.join('&')}`
}

export function logsUrl(cfg, limit, offset) {
  return `/api/v1/${encodeURIComponent(cfg.agent)}/logs?limit=${encodeURIComponent(String(limit))}&offset=${encodeURIComponent(String(offset))}`
}

export function sessionsUrl(cfg) {
  return `/api/v1/${encodeURIComponent(cfg.agent)}/sessions`
}

export function ingestBody(sessionId, turns) {
  return {
    session_id: sessionId,
    turn_count: turns.length,
    raw_turns: JSON.stringify(turns),
  }
}
