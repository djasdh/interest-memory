#!/usr/bin/env node
/**
 * interest-memory MCP server.
 *
 * Exposes the interest-memory consumer tools (`memory_search`, `memory_logs`)
 * over MCP stdio. Shared by all MCP clients (codex, claude-code, reasonix);
 * the agent namespace comes from `INTEREST_AGENT` (set per client in the MCP
 * config), and the service URL from `INTEREST_BASE_URL`.
 *
 * Config via environment variables:
 *   INTEREST_BASE_URL  — service base URL (default: http://127.0.0.1:8899)
 *   INTEREST_AGENT     — agent namespace (default: "default")
 *   INTEREST_TIMEOUT   — per-request timeout seconds (default: 8)
 *
 * Run: node bridge/mcp-server/server.ts
 */
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { z } from "zod"
import { memoryConfig, memoryLogs, memorySearch } from "./lib.ts"

const cfg = memoryConfig()

const server = new McpServer({
  name: "interest-memory",
  version: "1.0.0",
})

// INTEREST_MODE=input exposes no tools (write-only mode); output/auto expose
// the read-side tools.
if (cfg.mode !== "input") {
  server.registerTool(
    "memory_search",
    {
      description:
        "Search the interest-memory knowledge base and return full entries (body/claims/evidence) with their relationship edges. Pass 'query' for a semantic search, or 'id' to fetch one specific page/interest point by id (id wins when both are given).",
      inputSchema: {
        query: z.string().optional().describe("Semantic search query (topic, decision, or phrase)"),
        id: z.string().optional().describe("Exact id of a wiki page or interest point to fetch"),
        top_k: z.number().int().min(1).optional().describe("Max results for query search (default 3)"),
      },
    },
    async (args) => {
      try {
        return { content: [{ type: "text" as const, text: await memorySearch(cfg, args) }] }
      } catch (err) {
        console.error("[mcp-server] memory_search error:", err)
        return { content: [{ type: "text" as const, text: JSON.stringify({ error: String(err) }) }] }
      }
    },
  )

  server.registerTool(
    "memory_logs",
    {
      description:
        "Query the change-log of the interest-memory knowledge base: recent structural changes (page/interest-point title, action, and edges touched), newest first.",
      inputSchema: {
        limit: z.number().int().min(0).optional().describe("Max log entries (default 10)"),
        offset: z.number().int().min(0).optional().describe("Pagination offset (default 0)"),
      },
    },
    async (args) => {
      try {
        return { content: [{ type: "text" as const, text: await memoryLogs(cfg, args) }] }
      } catch (err) {
        console.error("[mcp-server] memory_logs error:", err)
        return { content: [{ type: "text" as const, text: JSON.stringify({ error: String(err) }) }] }
      }
    },
  )
}

const transport = new StdioServerTransport()
await server.connect(transport)
