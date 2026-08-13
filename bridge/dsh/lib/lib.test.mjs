import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  normalizeConfig,
  textBlocks,
  isOwnedSnapshot,
  wireTurns,
  lastUserQuery,
  fingerprintOf,
  recallUrl,
  searchUrl,
  logsUrl,
  ingestBody,
} from './lib.js'

test('normalizeConfig fills defaults and strips trailing slash', () => {
  const cfg = normalizeConfig()
  assert.equal(cfg.baseUrl, 'http://127.0.0.1:8899')
  assert.equal(cfg.agent, 'dsh')
  assert.equal(normalizeConfig({ baseUrl: 'http://x:1/' }).baseUrl, 'http://x:1')
  assert.equal(normalizeConfig({ agent: 'codex', recallTimeoutMs: 9000 }).recallTimeoutMs, 9000)
})

test('textBlocks joins text blocks and trims the whole string', () => {
  assert.equal(textBlocks([{ type: 'text', text: 'a' }, { type: 'text', text: ' b ' }]), 'a\n b')
  assert.equal(textBlocks([{ type: 'text', text: ' x ' }]), 'x')
  assert.equal(textBlocks([{ type: 'tool_result', text: 'x' }]), '')
  assert.equal(textBlocks(undefined), '')
})

test('isOwnedSnapshot matches only the runtime-context ownership marker', () => {
  assert.equal(isOwnedSnapshot({ source: { kind: 'plugin', plugin: '@deepseek-ai/dsh-system-prompt' } }), true)
  assert.equal(isOwnedSnapshot({ source: { kind: 'plugin', plugin: 'other' } }), false)
  assert.equal(isOwnedSnapshot({ source: { kind: 'user' } }), false)
  assert.equal(isOwnedSnapshot(null), false)
})

function msg(role, text, source) {
  const m = { role, content: [{ type: 'text', text }] }
  if (source) m.source = source
  return m
}

test('wireTurns drops owned snapshots, maps tool results, keeps user/assistant', () => {
  const messages = [
    msg('user', 'hello'),
    msg('assistant', 'hi'),
    { role: 'user', content: [{ type: 'tool-result', toolCallId: 'c1', content: [{ type: 'text', text: 'ls: out' }] }] },
    { role: 'user', content: [{ type: 'text', text: 'Current runtime context' }], source: { kind: 'plugin', plugin: '@deepseek-ai/dsh-system-prompt' } },
    msg('assistant', ''),
  ]
  const turns = wireTurns(messages)
  assert.deepEqual(turns, [
    { role: 'user', content: 'hello' },
    { role: 'assistant', content: 'hi' },
    { role: 'tool_result', content: 'ls: out' },
  ])
})

test('lastUserQuery skips snapshots and tool results', () => {
  const messages = [
    msg('user', 'first'),
    { role: 'user', content: [{ type: 'text', text: 'snapshot' }], source: { kind: 'plugin', plugin: '@deepseek-ai/dsh-system-prompt' } },
    { role: 'user', content: [{ type: 'tool_result', text: 'x' }] },
    msg('user', 'latest'),
  ]
  assert.equal(lastUserQuery(messages), 'latest')
})

test('fingerprintOf is stable and empty-safe', () => {
  assert.equal(fingerprintOf([]), '')
  const a = [{ role: 'user', content: 'x' }]
  assert.equal(fingerprintOf(a), '1:x')
  assert.equal(fingerprintOf([{ role: 'user', content: 'x' }]), fingerprintOf(a))
})

test('url builders escape and route by agent', () => {
  const cfg = normalizeConfig({ agent: 'my agent' })
  assert.equal(recallUrl(cfg, 'q/1'), '/api/v1/my%20agent/recall?query=q%2F1')
  assert.equal(searchUrl(cfg, { query: 'a b', top_k: 3 }), '/api/v1/my%20agent/search?query=a%20b&top_k=3')
  assert.equal(searchUrl(cfg, { id: 'x' }), '/api/v1/my%20agent/search?id=x')
  assert.equal(logsUrl(cfg, 5, 0), '/api/v1/my%20agent/logs?limit=5&offset=0')
})

test('ingestBody double-encodes raw_turns like the other bridges', () => {
  const body = ingestBody('s1', [{ role: 'user', content: 'hi' }])
  assert.equal(body.session_id, 's1')
  assert.equal(body.turn_count, 1)
  assert.deepEqual(JSON.parse(body.raw_turns), [{ role: 'user', content: 'hi' }])
})
