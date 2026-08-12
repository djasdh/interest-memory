#!/usr/bin/env node
/**
 * Unit tests for the interest-memory MCP server pure logic.
 *
 * Dependency-free: uses node:test + a tiny HTTP stub. Run:
 *   node --test bridge/mcp-server/lib.test.mjs
 */
import { test } from "node:test"
import assert from "node:assert"
import { createServer } from "node:http"
import { memoryConfig, memorySearch, memoryLogs } from "./lib.ts"

test("memoryConfig defaults", () => {
  const cfg = memoryConfig({})
  assert.equal(cfg.baseUrl, "http://127.0.0.1:8899")
  assert.equal(cfg.agent, "default")
  assert.equal(cfg.timeoutMs, 8000)
})

test("memoryConfig reads env", () => {
  const cfg = memoryConfig({ INTEREST_BASE_URL: "http://x:9/", INTEREST_AGENT: "a1", INTEREST_TIMEOUT: "3" })
  assert.equal(cfg.baseUrl, "http://x:9")
  assert.equal(cfg.agent, "a1")
  assert.equal(cfg.timeoutMs, 3000)
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

test("memoryConfig mode", () => {
  assert.equal(memoryConfig({}).mode, "auto")
  assert.equal(memoryConfig({ INTEREST_MODE: "input" }).mode, "input")
  assert.equal(memoryConfig({ INTEREST_MODE: "output" }).mode, "output")
  assert.equal(memoryConfig({ INTEREST_MODE: "bogus" }).mode, "auto")
})
