/**
 * @djasdh/interest-memory-dsh-bridge — official Cordis plugin for DeepSeek Harness.
 *
 * Thin REST adapter over the interest-memory service, in the same spirit as
 * the Hermes MemoryProvider bridge: config-driven, failure isolated (a dead
 * service never blocks a turn), and every effect registered through Cordis so
 * stop/unload tears it all down.
 *
 *   recall  — `agent/inbox/claimed` starts a recall fetch with the CURRENT
 *             user message; the `system-prompt/assemble` waterfall awaits it
 *             and pushes the memory as a context contribution, so the
 *             runtime-context projection materialises an OWNED user-role
 *             snapshot in this step's messages. Zero lag, no phantom
 *             messages, stable prefix (the snapshot is reused thereafter).
 *   ingest  — transcript is derived from the session log at session end
 *             (owned snapshots filtered out) and pushed once.
 *   tools   — memory_search / memory_logs / memory_ingest.
 *
 * Mount as an agent-preset row (per-session memory) or a host row
 * (shared service, state keyed per agent). See README.md.
 */
import z from '@deepseek-ai/schemastery'
import { defineTool } from '@deepseek-ai/dsh-tools'

import {
  normalizeConfig,
  textBlocks,
  wireTurns,
  lastUserQuery,
  fingerprintOf,
  recallUrl,
  searchUrl,
  logsUrl,
  sessionsUrl,
  ingestBody,
} from './lib.js'

/** Default row id when the composition omits `id:`. */
const name = 'interest-memory'

/** The tools registry is a hard dependency; the rest are optional services. */
const inject = ['tools']

const Config = z.object({
  baseUrl: z.string().default('http://127.0.0.1:8899'),
  agent: z.string().default('dsh'),
  recallTimeoutMs: z.number().default(5000),
  ingestTimeoutMs: z.number().default(15000),
  traceDir: z.string().default(''),
})

function apply(ctx, rawConfig) {
  const cfg = normalizeConfig(rawConfig ?? {})
  const shell = ctx.get('shell')
  const fs = ctx.get('fs')
  const sessions = ctx.get('sessions')
  const systemPrompt = ctx.get('systemPrompt')
  if (shell === undefined) return

  // ── observability (traceDir empty = off) ───────────────────────────────
  async function writeTrace(file, content) {
    if (!cfg.traceDir || !fs) return
    try {
      const target = await fs.resolve(cfg.traceDir + '/' + file)
      await fs.writeText(target, JSON.stringify({ at: new Date().toISOString(), ...content }, null, 2))
    } catch (err) {
      console.error('interest-memory trace write:', err)
    }
  }

  // ── HTTP client: shell + curl; POST bodies ride stdin (no shell quoting) ─
  async function http(method, path, body, timeoutMs, signal) {
    const ms = timeoutMs ?? 8000
    const seconds = Math.max(2, Math.floor(ms / 1000))
    const url = cfg.baseUrl + path
    const command =
      method === 'GET'
        ? `curl -s -m ${seconds} '${url}'`
        : `curl -s -m ${seconds} -X POST -H 'Content-Type: application/json' --data-binary @- '${url}'`
    const spec = shell.resolve({
      command,
      timeoutMs: ms,
      stdoutMaxBytes: 262144,
      stdin: body === undefined ? undefined : JSON.stringify(body),
      signal,
    })
    const res = await shell.run(spec)
    return { ok: res.exitCode === 0, stdout: res.stdout.text }
  }

  // ── recall: dual-seam zero-lag injection, state keyed per agent so the
  //    row is correct both mounted host-wide and per preset ────────────────
  const recallByAgent = new Map()

  ctx.on('agent/inbox/claimed', (payload) => {
    const agentId = payload && payload.agent ? payload.agent.id : undefined
    if (!agentId) return
    const state = recallByAgent.get(agentId)
    if (state && (state.hydrated || state.hydrating)) return
    const query = payload && payload.message ? textBlocks(payload.message.content) : ''
    if (!query) return
    const task = (async () => {
      const res = await http('GET', recallUrl(cfg, query), undefined, cfg.recallTimeoutMs)
      let text = ''
      let hydrated = false
      if (res.ok && res.stdout) {
        try {
          const parsed = JSON.parse(res.stdout)
          if (parsed && parsed.memory_context) {
            text = parsed.memory_context
            hydrated = true
          }
        } catch (err) {
          /* non-JSON body */
        }
      }
      const next = recallByAgent.get(agentId) ?? {}
      next.hydrating = null
      next.hydrated = hydrated
      next.text = text
      recallByAgent.set(agentId, next)
      await writeTrace('recall-last.json', {
        agentId,
        query,
        httpOk: res.ok,
        hydrated,
        chars: text.length,
      })
      console.log(`interest-memory recall: ${text.length} chars for ${agentId}: ${query.slice(0, 50)}`)
    })()
    recallByAgent.set(agentId, { hydrating: task, hydrated: false, text: '' })
  })

  if (systemPrompt !== undefined) {
    ctx.on('system-prompt/assemble', async (assembly, context, next) => {
      try {
        const base = await next()
        const agentId = context && context.agent ? context.agent.id : undefined
        if (agentId) {
          const state = recallByAgent.get(agentId)
          if (state && state.hydrating) await state.hydrating
          if (state && state.text && base.contexts) {
            base.contexts.push({
              name: 'interest-memory:recall',
              text: `<memory_context>\n${state.text}\n</memory_context>`,
            })
            await writeTrace('assemble-injected.json', { agentId, chars: state.text.length })
          }
        }
        return base
      } catch (err) {
        console.error('interest-memory assemble:', err)
        return next()
      }
    })
  }

  // ── ingest: session-end transcript push with resume protection ──────────
  const pushedBySession = new Map()

  async function ingestNow(sessionId) {
    const sess = sessions ? sessions.get(sessionId) : undefined
    const messages = sess && sess.deriveMessages ? sess.deriveMessages() : []
    const turns = wireTurns(messages)
    if (turns.length === 0) return { skipped: true, turns: 0 }
    const fp = fingerprintOf(turns)
    if (pushedBySession.get(sessionId) === fp) return { skipped: true, turns: turns.length }
    pushedBySession.set(sessionId, fp)
    const res = await http(
      'POST',
      sessionsUrl(cfg),
      ingestBody(sessionId, turns),
      cfg.ingestTimeoutMs,
    )
    const result = { ok: res.ok, turns: turns.length, detail: res.stdout.slice(0, 200) }
    await writeTrace('ingest-last.json', { sessionId, ...result })
    console.log('interest-memory ingest:', JSON.stringify(result))
    return result
  }

  ctx.on('session/disposed', (session) => {
    void ingestNow(session.id).catch((err) => console.error('interest-memory ingest:', err))
  })

  // ── model tools ─────────────────────────────────────────────────────────
  function output() {
    return {
      schema: { type: 'object', additionalProperties: true },
      render(args, value) {
        return [{ type: 'text', text: JSON.stringify(value) }]
      },
    }
  }

  ctx.tools.register(
    defineTool({
      name: 'memory_search',
      description:
        'Search the interest-memory long-term knowledge base. Items carry title, body, reliability, freshness and graph edges (outlinks/backlinks); pass an id to jump to a node and walk the memory graph.',
      parameters: {
        type: 'object',
        properties: {
          query: { type: 'string', description: 'natural-language or keyword query' },
          id: { type: 'string', description: 'node id to fetch and expand (overrides query)' },
          top_k: { type: 'number', description: 'max results (default 3)' },
        },
      },
      output: output(),
      async execute(args, exec) {
        const res = await http('GET', searchUrl(cfg, args), undefined, 10000, exec.signal)
        await writeTrace('tool-search-last.json', { args, httpOk: res.ok })
        if (!res.ok) return { error: 'memory_search: curl exit ' + String(res.exitCode) }
        try {
          return JSON.parse(res.stdout)
        } catch (err) {
          return { error: 'memory_search: invalid JSON response' }
        }
      },
    }),
  )

  ctx.tools.register(
    defineTool({
      name: 'memory_logs',
      description:
        'List recent structural changes to the interest-memory knowledge base (change_log, newest first).',
      parameters: {
        type: 'object',
        properties: {
          limit: { type: 'number', description: 'max entries (default 10)' },
          offset: { type: 'number', description: 'pagination offset (default 0)' },
        },
      },
      output: output(),
      async execute(args, exec) {
        const limit = args.limit !== undefined && Number.isFinite(args.limit) ? args.limit : 10
        const offset = args.offset !== undefined && Number.isFinite(args.offset) ? args.offset : 0
        const res = await http('GET', logsUrl(cfg, limit, offset), undefined, 10000, exec.signal)
        await writeTrace('tool-logs-last.json', { args, httpOk: res.ok })
        if (!res.ok) return { error: 'memory_logs: curl exit ' + String(res.exitCode) }
        try {
          return JSON.parse(res.stdout)
        } catch (err) {
          return { error: 'memory_logs: invalid JSON response' }
        }
      },
    }),
  )

  ctx.tools.register(
    defineTool({
      name: 'memory_ingest',
      description:
        "Force a checkpoint: push this session's transcript so far into interest-memory for extraction (normally automatic on session end). Returns whether the ingest was accepted.",
      parameters: { type: 'object', properties: {} },
      output: output(),
      async execute(args, exec) {
        const agents = ctx.get('agents')
        const agent = agents ? agents.currentInitiator() : undefined
        const id = agent ? agent.id : undefined
        if (!id) return { skipped: true, reason: 'no initiator' }
        return ingestNow(id)
      },
    }),
  )
}

export { Config, apply, inject, name }
