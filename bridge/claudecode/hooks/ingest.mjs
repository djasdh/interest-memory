#!/usr/bin/env node
/**
 * interest-memory — Claude Code SessionEnd hook.
 *
 * Reads the session transcript (`transcript_path` from stdin) and pushes it to
 * `POST /api/v1/{agent}/sessions`. Best-effort within the hook budget; never
 * throws. On failure the backend keeps the transcript for manual retry.
 */
import { ingest, memoryConfig, parseClaudeTranscript, pushedKey, setPushedKey } from "./lib.mjs"

let input = ""
process.stdin.on("data", (c) => (input += c))
process.stdin.on("end", async () => {
  try {
    const cfg = memoryConfig()
    if (cfg.mode === "output") process.exit(0) // output-only: no ingest
    const ev = JSON.parse(input || "{}")
    const transcriptPath = typeof ev.transcript_path === "string" ? ev.transcript_path : ""
    const sessionID = typeof ev.session_id === "string" ? ev.session_id : ""
    if (!transcriptPath || !sessionID) process.exit(0)
    const turns = parseClaudeTranscript(transcriptPath)
    if (!turns.length) process.exit(0)
    // Resume protection: skip when this session's transcript is unchanged.
    const lastKey = `${turns.length}:${turns[turns.length - 1].content.slice(0, 200)}`
    if (lastKey === pushedKey(cfg.agent, sessionID)) process.exit(0)
    await ingest(cfg, sessionID, turns, new Date().toISOString())
    setPushedKey(cfg.agent, sessionID, lastKey)
    process.exit(0)
  } catch {
    process.exit(0)
  }
})
