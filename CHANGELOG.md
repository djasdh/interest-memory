# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-08

Initial open-source release.

### Added

- **Memory pipeline** — session-end transcript ingestion: prefix-window
  parallel extraction → three-stage correction (subjective exemption,
  relation verdict, evidence locators) → per-interest-point wiki write →
  related-page reconciliation (≤3-hop graph propagation).
- **Recall** — session-start RAG injection with time filtering
  (`after`/`before`/`days`) and source-namespace annotation.
- **Full audit trail** — `change_log` records every structural change
  (title + action + structural edges); `ListTags` aggregation + `wiki_tags`
  tool.
- **Cross-namespace sharing** — read-side `isolated`/`all`/`custom`
  visibility modes (writes always isolated).
- **Agent bridges** — Hermes (`MemoryProvider` plugin), opencode, pi,
  openclaw, claudecode, codex, reasonix, plus a shared MCP server exposing
  `memory_search` / `memory_logs`.
- **Guided installer** — `scripts/install.sh` with per-distro dependency
  checks, curses TUI, systemd autostart, and non-interactive LLM
  configuration (`scripts/install_llm.py`).
- **Bilingual docs** — README in English and Chinese with architecture &
  data-flow diagrams, REST API reference, and resource benchmark.
- **Tests** — per-package Go unit tests (fake LLM), dependency-free
  node:test suites for every bridge, and a real-LLM end-to-end script.

[0.1.0]: https://github.com/djasdh/interest-memory/releases/tag/v0.1.0
