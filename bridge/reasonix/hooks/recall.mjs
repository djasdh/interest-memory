#!/usr/bin/env node
/**
 * interest-memory — Reasonix UserPromptSubmit hook.
 *
 * Injects `GET /api/v1/{agent}/recall?query=<user prompt>` context on every
 * user prompt submission. Reads the hook event JSON from stdin and prints the
 * memory context to stdout. Never throws; failures degrade to a no-op.
 */
import { memoryConfig, recall } from "./lib.mjs"

let input = ""
process.stdin.on("data", (c) => (input += c))
process.stdin.on("end", async () => {
  try {
    const ev = JSON.parse(input || "{}")
    const query = typeof ev.prompt === "string" ? ev.prompt.trim() : ""
    if (!query) process.exit(0)
    const ctx = await recall(memoryConfig(), query)
    if (!ctx) process.exit(0)
    process.stdout.write(`<memory_context>\n${ctx}\n</memory_context>`)
    process.exit(0)
  } catch {
    process.exit(0)
  }
})
