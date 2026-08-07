#!/usr/bin/env node
/**
 * Unit tests for the Codex interest-memory bridge pure logic.
 *
 * Dependency-free: node:test + a tiny HTTP stub. Run:
 *   node --test bridge/codex/hooks/lib.test.mjs
 */
import { test } from "node:test"
import assert from "node:assert"
import { createServer } from "node:http"
import { mkdtempSync, writeFileSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { memoryConfig, recall, ingest, parseCodexTranscript } from "./lib.mjs"

test("memoryConfig defaults and env", () => {
  assert.equal(memoryConfig({}).agent, "codex")
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

test("parseCodexTranscript extracts user/assistant turns", () => {
  const dir = mkdtempSync(join(tmpdir(), "im-cx-"))
  const path = join(dir, "t.jsonl")
  const line = (type, payload) => JSON.stringify({ type, payload })
  writeFileSync(
    path,
    [
      line("session_meta", { session_id: "s1" }),
      line("response_item", { type: "message", id: "m1", role: "developer", content: [{ type: "input_text", text: "sys" }] }),
      line("response_item", { type: "message", id: "m2", role: "user", content: [{ type: "input_text", text: "hello" }] }),
      line("response_item", { type: "message", id: "m3", role: "assistant", content: [{ type: "output_text", text: "hi" }] }),
      line("response_item", { type: "message", id: "m4", role: "assistant", content: [{ type: "function_call", name: "Bash" }] }),
      line("response_item", { type: "message", id: "m5", role: "user", content: [{ type: "input_text", text: "" }] }),
      "not json",
    ].join("\n"),
  )
  try {
    const turns = parseCodexTranscript(path)
    assert.deepEqual(turns, [
      { role: "user", content: "hello" },
      { role: "assistant", content: "hi" },
    ])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test("parseCodexTranscript missing file returns empty", () => {
  assert.deepEqual(parseCodexTranscript("/nonexistent/x.jsonl"), [])
})
