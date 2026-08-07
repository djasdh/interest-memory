#!/usr/bin/env node
/**
 * interest-memory — Reasonix SessionEnd hook.
 *
 * Reads the session transcript and pushes it to `POST /api/v1/{agent}/sessions`.
 * Prefers the `transcript_path` from the hook event stdin; falls back to the
 * newest Reasonix session jsonl under the workspace. Best-effort; never throws.
 */
import { ingest, memoryConfig, parseReasonixTranscript } from "./lib.mjs"
import { readdirSync, statSync } from "node:fs"
import { join } from "node:path"

function newestSessionFile(root) {
  try {
    const dir = join(root, "sessions")
    const files = readdirSync(dir).filter((f) => f.endsWith(".jsonl") && !f.endsWith(".events.jsonl"))
    let best = ""
    let bestMtime = 0
    for (const f of files) {
      const p = join(dir, f)
      const st = statSync(p)
      if (st.mtimeMs > bestMtime) {
        bestMtime = st.mtimeMs
        best = p
      }
    }
    return best
  } catch {
    return ""
  }
}

let input = ""
process.stdin.on("data", (c) => (input += c))
process.stdin.on("end", async () => {
  try {
    const ev = JSON.parse(input || "{}")
    let transcriptPath = typeof ev.transcript_path === "string" ? ev.transcript_path : ""
    const sessionID = typeof ev.session_id === "string" ? ev.session_id : ""
    if (!transcriptPath && process.env.REASONIX_HOME) {
      transcriptPath = newestSessionFile(process.env.REASONIX_HOME)
    }
    if (!transcriptPath || !sessionID) process.exit(0)
    const turns = parseReasonixTranscript(transcriptPath)
    if (!turns.length) process.exit(0)
    await ingest(memoryConfig(), sessionID, turns, new Date().toISOString())
    process.exit(0)
  } catch {
    process.exit(0)
  }
})
