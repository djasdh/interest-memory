#!/usr/bin/env node
/**
 * Unit tests for the pi interest-memory bridge pure logic.
 *
 * Dependency-free: uses node:test + a tiny HTTP stub. Run:
 *   node --test bridge/pi/lib.test.mjs
 */
import { test } from "node:test"
import assert from "node:assert"
import { createServer } from "node:http"
import { memoryConfig, extractTurns, lastUserText, recall, ingest, memorySearch, memoryLogs } from "./lib.ts"

test("memoryConfig defaults", () => {
  const cfg = memoryConfig({})
  assert.equal(cfg.baseUrl, "http://127.0.0.1:8899")
  assert.equal(cfg.agent, "pi")
  assert.equal(cfg.timeoutMs, 8000)
})

test("memoryConfig reads env", () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://x:9/", INTEREST_AGENT: "a1", INTEREST_TIMEOUT: "3" })
  assert.equal(cfg.baseUrl, "http://x:9")
  assert.equal(cfg.agent, "a1")
  assert.equal(cfg.timeoutMs, 3000)
})

test("extractTurns from session entries (message entries)", () => {
  const entries = [
    { type: "message", message: { role: "user", content: "prefer golang" } },
    { type: "message", message: { role: "assistant", content: [{ type: "text", text: "ok" }, { type: "thinking", text: "x" }] } },
    { type: "message", message: { role: "toolResult", toolName: "bash", content: [{ type: "text", text: "done" }] } },
    { type: "custom", customType: "plan-mode" },
    { type: "message", message: { role: "user", content: "" } },
  ]
  const turns = extractTurns(entries)
  assert.deepEqual(turns, [
    { role: "user", content: "prefer golang" },
    { role: "assistant", content: "ok" },
    { role: "tool_result", content: "bash: done" },
  ])
})

test("extractTurns from raw agent messages (no type field)", () => {
  const turns = extractTurns([
    { role: "user", content: [{ type: "text", text: "a" }] },
    { role: "assistant", content: "b" },
    { role: "system", content: "ignored" },
  ])
  assert.deepEqual(turns, [
    { role: "user", content: "a" },
    { role: "assistant", content: "b" },
  ])
})

test("lastUserText finds last user text", () => {
  const entries = [
    { type: "message", message: { role: "user", content: "first" } },
    { type: "message", message: { role: "assistant", content: "mid" } },
    { type: "message", message: { role: "user", content: "last" } },
  ]
  assert.equal(lastUserText(entries), "last")
  assert.equal(lastUserText([{ type: "message", message: { role: "user", content: "" } }]), "")
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
    assert.equal(await recall(cfg, "golang"), "- Go [interest_point]")
  })
})

test("recall failure isolated", async () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://127.0.0.1:1" })
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
    assert.equal(payload.session_date, "2026-08-07T00:00:00.000Z")
    res.statusCode = 202
    res.end("{}")
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    const ok = await ingest(cfg, "s1", [
      { role: "user", content: "u1" },
      { role: "assistant", content: "a1" },
    ], "2026-08-07T00:00:00.000Z")
    assert.equal(ok, true)
  })
})

test("ingest empty short-circuits", async () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://127.0.0.1:1" })
  assert.equal(await ingest(cfg, "s1", []), false)
})

test("memorySearch query params", async () => {
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

test("memorySearch id precedence and missing-args isolation", async () => {
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
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://127.0.0.1:1" })
  assert.ok(JSON.parse(await memorySearch(cfg, {})).error)
})

test("memoryLogs params", async () => {
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
