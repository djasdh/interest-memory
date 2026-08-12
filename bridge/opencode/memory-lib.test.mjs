#!/usr/bin/env node
/**
 * Unit tests for the opencode interest-memory bridge pure logic.
 *
 * Dependency-free: uses node:test + a tiny HTTP stub. Run:
 *   node --test bridge/opencode/memory-lib.test.mjs
 */
import { test } from "node:test"
import assert from "node:assert"
import { createServer } from "node:http"
import {
  memoryConfig,
  extractTurns,
  lastUserText,
  recall,
  ingest,
  memorySearch,
  memoryLogs,
  buildMemoryTurn,
  cacheSnapshot,
  pushedKey,
  setPushedKey,
} from "./memory-lib.ts"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { mkdtempSync, rmSync } from "node:fs"

test("memoryConfig defaults", () => {
  const cfg = memoryConfig({})
  assert.equal(cfg.baseUrl, "http://127.0.0.1:8899")
  assert.equal(cfg.agent, "opencode")
  assert.equal(cfg.timeoutMs, 8000)
})

test("memoryConfig reads env", () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://x:9/", INTEREST_AGENT: "a1", INTEREST_TIMEOUT: "3" })
  assert.equal(cfg.baseUrl, "http://x:9")
  assert.equal(cfg.agent, "a1")
  assert.equal(cfg.timeoutMs, 3000)
})

test("extractTurns reduces opencode messages to wire turns", () => {
  const messages = [
    { info: { role: "user", id: "u1" }, parts: [{ type: "text", text: "prefer golang" }] },
    { info: { role: "assistant", id: "a1" }, parts: [{ type: "text", text: "ok" }] },
    {
      info: { role: "assistant", id: "a2" },
      parts: [
        { type: "tool", tool: "bash", state: { status: "completed", output: "done" } },
        { type: "text", text: "ran it" },
      ],
    },
    { info: { role: "user", id: "u2" }, parts: [{ type: "text", text: "" }] },
  ]
  const turns = extractTurns(messages)
  assert.deepEqual(turns, [
    { role: "user", content: "prefer golang" },
    { role: "assistant", content: "ok" },
    { role: "assistant", content: "ran it" },
    { role: "tool_result", content: "bash: done" },
  ])
})

test("extractTurns drops non user/assistant roles and empty text", () => {
  const turns = extractTurns([
    { info: { role: "system", id: "s" }, parts: [{ type: "text", text: "x" }] },
    { info: { role: "user", id: "u" }, parts: [{ type: "text", text: "   " }] },
  ])
  assert.deepEqual(turns, [])
})

test("lastUserText finds the last non-empty user text", () => {
  const messages = [
    { info: { role: "user" }, parts: [{ type: "text", text: "first" }] },
    { info: { role: "assistant" }, parts: [{ type: "text", text: "mid" }] },
    { info: { role: "user" }, parts: [{ type: "text", text: "last" }] },
  ]
  assert.equal(lastUserText(messages), "last")
  assert.equal(lastUserText([{ info: { role: "user" }, parts: [{ type: "text", text: "" }] }]), "")
})

async function withStubServer(handler, fn) {
  const server = createServer(handler)
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  const { port } = server.address()
  try {
    await fn(`http://127.0.0.1:${port}`)
  } finally {
    await new Promise((resolve) => server.close(resolve))
  }
}

test("recall returns bare memory_context", async () => {
  await withStubServer((req, res) => {
    assert.ok(req.url.includes("/api/v1/agent-a/recall"))
    assert.ok(req.url.includes("query=golang"))
    res.setHeader("Content-Type", "application/json")
    res.end(JSON.stringify({ memory_context: "- Go [interest_point]" }))
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    const out = await recall(cfg, "golang")
    assert.equal(out, "- Go [interest_point]")
  })
})

test("recall failure is isolated", async () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://127.0.0.1:1", INTEREST_AGENT: "agent-a" })
  assert.equal(await recall(cfg, "q"), "")
  assert.equal(await recall(cfg, ""), "")
})

test("ingest posts transcript with session_date", async () => {
  await withStubServer(async (req, res) => {
    assert.equal(req.method, "POST")
    assert.ok(req.url.includes("/api/v1/agent-a/sessions"))
    let body = ""
    for await (const chunk of req) body += chunk
    const payload = JSON.parse(body)
    assert.equal(payload.session_id, "s1")
    assert.equal(payload.turn_count, 2)
    assert.deepEqual(JSON.parse(payload.raw_turns), [
      { role: "user", content: "u1" },
      { role: "assistant", content: "a1" },
    ])
    assert.equal(payload.session_date, undefined) // not passed → omitted
    res.statusCode = 202
    res.end(JSON.stringify({ job_id: "j1" }))
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    const ok = await ingest(cfg, "s1", [
      { role: "user", content: "u1" },
      { role: "assistant", content: "a1" },
    ])
    assert.equal(ok, true)
  })
})

test("ingest includes session_date when provided", async () => {
  await withStubServer(async (req, res) => {
    let body = ""
    for await (const chunk of req) body += chunk
    const payload = JSON.parse(body)
    assert.equal(payload.session_date, "2026-08-07T00:00:00.000Z")
    res.statusCode = 202
    res.end("{}")
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    const ok = await ingest(cfg, "s1", [{ role: "user", content: "u1" }], "2026-08-07T00:00:00.000Z")
    assert.equal(ok, true)
  })
})

test("ingest empty turns short-circuits", async () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://127.0.0.1:1" })
  assert.equal(await ingest(cfg, "s1", []), false)
})

test("memorySearch query builds correct params", async () => {
  await withStubServer((req, res) => {
    assert.ok(req.url.includes("/api/v1/agent-a/search"))
    assert.ok(req.url.includes("query=postgresql"))
    assert.ok(req.url.includes("top_k=5"))
    res.setHeader("Content-Type", "application/json")
    res.end(JSON.stringify({ items: [{ id: "pg" }] }))
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    const out = await memorySearch(cfg, { query: "postgresql", top_k: 5 })
    assert.deepEqual(JSON.parse(out), [{ id: "pg" }])
  })
})

test("memorySearch id takes precedence and omits query", async () => {
  await withStubServer((req, res) => {
    assert.ok(req.url.includes("id=ip-1"))
    assert.ok(!req.url.includes("query="))
    res.setHeader("Content-Type", "application/json")
    res.end(JSON.stringify({ items: [{ id: "ip-1" }] }))
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    const out = await memorySearch(cfg, { query: "x", id: "ip-1" })
    assert.deepEqual(JSON.parse(out), [{ id: "ip-1" }])
  })
})

test("memorySearch missing args and failure are isolated", async () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://127.0.0.1:1" })
  const missing = JSON.parse(await memorySearch(cfg, {}))
  assert.ok(missing.error)
  const failed = JSON.parse(await memorySearch(cfg, { query: "q" }))
  assert.ok(failed.error)
})

test("memoryLogs pagination params", async () => {
  await withStubServer((req, res) => {
    assert.ok(req.url.includes("/api/v1/agent-a/logs"))
    assert.ok(req.url.includes("limit=5"))
    assert.ok(req.url.includes("offset=2"))
    res.setHeader("Content-Type", "application/json")
    res.end(JSON.stringify({ items: [{ id: "l1" }] }))
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    const out = await memoryLogs(cfg, { limit: 5, offset: 2 })
    assert.deepEqual(JSON.parse(out), [{ id: "l1" }])
  })
})

test("memoryLogs failure isolated", async () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://127.0.0.1:1" })
  const out = JSON.parse(await memoryLogs(cfg, {}))
  assert.ok(out.error)
})

test("buildMemoryTurn clones base info and wraps memory_context", () => {
  const base = { id: "u1", role: "user", sessionID: "s1" }
  const turn = buildMemoryTurn(base, "- Go [interest_point]")
  assert.equal(turn.info.id.startsWith("memory-recall-"), true)
  assert.equal(turn.info.role, "user")
  assert.equal(turn.info.sessionID, "s1")
  assert.equal(turn.parts.length, 1)
  assert.equal(turn.parts[0].type, "text")
  assert.equal(turn.parts[0].text, "<memory_context>\n- Go [interest_point]\n</memory_context>")
})

test("buildMemoryTurn tolerates missing base info", () => {
  const turn = buildMemoryTurn(undefined, "ctx")
  assert.equal(turn.info.id.startsWith("memory-recall-"), true)
  assert.equal(turn.info.role, undefined)
})

test("cacheSnapshot is a copy unaffected by later splice", () => {
  const messages = [
    { info: { role: "user" }, parts: [{ type: "text", text: "u1" }] },
    { info: { role: "assistant" }, parts: [{ type: "text", text: "a1" }] },
  ]
  const snapshot = cacheSnapshot(messages)
  // Simulate the transform hook splicing the recall turn in-place.
  messages.splice(messages.length - 1, 0, {
    info: { role: "user", id: "memory-recall-123" },
    parts: [{ type: "text", text: "<memory_context>\nctx\n</memory_context>" }],
  })
  assert.equal(messages.length, 3)
  assert.equal(snapshot.length, 2)
  assert.equal(snapshot[0].parts[0].text, "u1")
  assert.equal(snapshot[1].parts[0].text, "a1")
  // The snapshot must not contain the injected turn (would pollute ingest).
  assert.equal(snapshot.some((m) => m.info?.id?.startsWith("memory-recall-")), false)
})

test("memoryConfig mode", () => {
  assert.equal(memoryConfig({}).mode, "auto")
  assert.equal(memoryConfig({ INTEREST_MODE: "input" }).mode, "input")
  assert.equal(memoryConfig({ INTEREST_MODE: "output" }).mode, "output")
  assert.equal(memoryConfig({ INTEREST_MODE: "bogus" }).mode, "auto")
})

test("pushedKey persists and caps at 10 sessions", () => {
  const dir = mkdtempSync(join(tmpdir(), "interest-state-"))
  process.env.INTEREST_STATE_FILE = join(dir, "state.json")
  const cfg = memoryConfig({ INTEREST_AGENT: "agent-a" })
  try {
    assert.equal(pushedKey(cfg, "s1"), "")
    setPushedKey(cfg, "s1", "key-1")
    assert.equal(pushedKey(cfg, "s1"), "key-1")
    for (let i = 2; i <= 11; i++) setPushedKey(cfg, `s${i}`, `key-${i}`)
    assert.equal(pushedKey(cfg, "s1"), "")
    assert.equal(pushedKey(cfg, "s2"), "key-2")
    assert.equal(pushedKey(cfg, "s11"), "key-11")
  } finally {
    delete process.env.INTEREST_STATE_FILE
    rmSync(dir, { recursive: true, force: true })
  }
})
