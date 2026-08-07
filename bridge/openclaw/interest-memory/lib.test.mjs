#!/usr/bin/env node
/**
 * Unit tests for the openclaw interest-memory bridge pure logic.
 *
 * Dependency-free: uses node:test + a tiny HTTP stub. Run:
 *   node --test bridge/openclaw/interest-memory/lib.test.mjs
 */
import { test } from "node:test";
import assert from "node:assert";
import { createServer } from "node:http";
import {
  resolveConfig,
  extractTurns,
  textOf,
  parseMemoryContext,
  recall,
  ingest,
  memorySearch,
  memoryLogs,
} from "./lib.ts";

test("resolveConfig defaults", () => {
  const cfg = resolveConfig({});
  assert.equal(cfg.baseUrl, "http://127.0.0.1:8899");
  assert.equal(cfg.agent, "default");
  assert.equal(cfg.timeoutMs, 8000);
});

test("resolveConfig reads env", () => {
  const cfg = resolveConfig({ INTEREST_BASE_URL: "http://x:9/", INTEREST_AGENT: "a1", INTEREST_TIMEOUT: "3" });
  assert.equal(cfg.baseUrl, "http://x:9");
  assert.equal(cfg.agent, "a1");
  assert.equal(cfg.timeoutMs, 3000);
});

test("textOf handles string and structured content", () => {
  assert.equal(textOf(" hello "), "hello");
  assert.equal(textOf([{ type: "text", text: "a" }, { type: "text", text: "b" }, { type: "image", text: "x" }]), "a\nb");
  assert.equal(textOf(undefined), "");
  assert.equal(textOf([]), "");
});

test("extractTurns maps openclaw AgentMessage[] to wire turns", () => {
  const turns = extractTurns([
    { role: "user", content: "prefer golang" },
    { role: "assistant", content: [{ type: "text", text: "ok" }] },
    { role: "toolResult", toolName: "bash", content: [{ type: "text", text: "done" }] },
    { role: "user", content: "" },
  ]);
  assert.deepEqual(turns, [
    { role: "user", content: "prefer golang" },
    { role: "assistant", content: "ok" },
    { role: "tool_result", content: "bash: done" },
  ]);
});

test("parseMemoryContext extracts memory_context", () => {
  assert.equal(parseMemoryContext('{"memory_context":"- Go"}'), "- Go");
  assert.equal(parseMemoryContext("not json"), "");
});

async function withStubServer(handler, fn) {
  const server = createServer(handler);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  try {
    await fn(`http://127.0.0.1:${port}`);
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
}

test("recall returns bare text", async () => {
  await withStubServer((req, res) => {
    assert.ok(req.url.includes("/api/v1/agent-a/recall"));
    assert.ok(req.url.includes("query=golang"));
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ memory_context: "- Go [interest_point]" }));
  }, async (base) => {
    assert.equal(await recall(base, "agent-a", "golang", 8000), "- Go [interest_point]");
  });
});

test("recall failure isolated", async () => {
  assert.equal(await recall("http://127.0.0.1:1", "a", "q", 100), "");
  assert.equal(await recall("http://127.0.0.1:1", "a", "", 100), "");
});

test("ingest posts transcript", async () => {
  await withStubServer(async (req, res) => {
    assert.equal(req.method, "POST");
    assert.ok(req.url.includes("/api/v1/agent-a/sessions"));
    let body = "";
    for await (const chunk of req) body += chunk;
    const payload = JSON.parse(body);
    assert.equal(payload.session_id, "s1");
    assert.equal(payload.turn_count, 2);
    assert.deepEqual(JSON.parse(payload.raw_turns), [{ role: "user", content: "u1" }, { role: "assistant", content: "a1" }]);
    assert.ok(payload.session_date);
    res.statusCode = 202;
    res.end("{}");
  }, async (base) => {
    const ok = await ingest(base, "agent-a", "s1", [
      { role: "user", content: "u1" },
      { role: "assistant", content: "a1" },
    ], new Date().toISOString());
    assert.equal(ok, true);
  });
});

test("ingest empty short-circuits", async () => {
  assert.equal(await ingest("http://x:1", "a", "s", [], undefined), false);
});

test("memorySearch query params", async () => {
  await withStubServer((req, res) => {
    assert.ok(req.url.includes("/api/v1/agent-a/search"));
    assert.ok(req.url.includes("query=postgresql"));
    assert.ok(req.url.includes("top_k=5"));
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ items: [{ id: "pg" }] }));
  }, async (base) => {
    const out = await memorySearch(base, "agent-a", { query: "postgresql", top_k: 5 }, 8000);
    assert.deepEqual(JSON.parse(out), [{ id: "pg" }]);
  });
});

test("memorySearch missing args isolated", async () => {
  const out = await memorySearch("http://127.0.0.1:1", "a", {}, 100);
  assert.ok(JSON.parse(out).error);
});

test("memoryLogs params", async () => {
  await withStubServer((req, res) => {
    assert.ok(req.url.includes("/api/v1/agent-a/logs"));
    assert.ok(req.url.includes("limit=5"));
    assert.ok(req.url.includes("offset=2"));
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ items: [{ id: "l1" }] }));
  }, async (base) => {
    const out = await memoryLogs(base, "agent-a", { limit: 5, offset: 2 }, 8000);
    assert.deepEqual(JSON.parse(out), [{ id: "l1" }]);
  });
});
