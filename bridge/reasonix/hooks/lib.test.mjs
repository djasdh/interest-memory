#!/usr/bin/env node
/**
 * Unit tests for the Reasonix interest-memory bridge pure logic.
 *
 * Dependency-free: node:test + a tiny HTTP stub. Run:
 *   node --test bridge/reasonix/hooks/lib.test.mjs
 */
import { test } from "node:test"
import assert from "node:assert"
import { createServer } from "node:http"
import { mkdtempSync, writeFileSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { memoryConfig, recall, ingest, parseReasonixTranscript } from "./lib.mjs"

test("memoryConfig defaults and env", () => {
  assert.equal(memoryConfig({}).agent, "reasonix")
  assert.equal(memoryConfig({ INTEREST_AGENT: "a1" }).agent, "a1")
  assert.equal(memoryConfig({}).baseUrl, "http://127.0.0.1:8899")
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
    assert.equal(await recall(cfg, ""), "")
  })
})

test("ingest posts transcript", async () => {
  await withStubServer(async (req, res) => {
    assert.equal(req.method, "POST")
    assert.ok(req.url.includes("/api/v1/agent-a/sessions"))
    let body = ""
    for await (const chunk of req) body += chunk
    const payload = JSON.parse(body)
    assert.equal(payload.session_id, "s1")
    assert.deepEqual(JSON.parse(payload.raw_turns), [
      { role: "user", content: "u1" },
      { role: "assistant", content: "a1" },
    ])
    res.statusCode = 202
    res.end("{}")
  }, async (base) => {
    const cfg = memoryConfig({ INTEREST_BASE_URL: base, INTEREST_AGENT: "agent-a" })
    assert.equal(
      await ingest(cfg, "s1", [
        { role: "user", content: "u1" },
        { role: "assistant", content: "a1" },
      ]),
      true,
    )
  })
})

test("parseReasonixTranscript extracts user/assistant turns", () => {
  const dir = mkdtempSync(join(tmpdir(), "im-rx-"))
  const path = join(dir, "t.jsonl")
  writeFileSync(
    path,
    [
      JSON.stringify({ role: "system", content: "sys" }),
      JSON.stringify({ role: "user", content: "hello" }),
      JSON.stringify({ role: "assistant", content: "hi", reasoning_content: "thinking" }),
      JSON.stringify({ role: "user", content: "" }),
      "not json",
    ].join("\n"),
  )
  try {
    const turns = parseReasonixTranscript(path)
    assert.deepEqual(turns, [
      { role: "user", content: "hello" },
      { role: "assistant", content: "hi" },
    ])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test("parseReasonixTranscript missing file returns empty", () => {
  assert.deepEqual(parseReasonixTranscript("/nonexistent/x.jsonl"), [])
})
