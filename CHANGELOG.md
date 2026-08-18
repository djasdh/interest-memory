# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.3] - 2026-08-18

### Added

- **Trusted reliability/freshness writeback** — `wiki_write` now accepts
  optional post-review parameters (`reliability_status`, `confidence`,
  `freshness_level`, `ttl_days`, `evidence`). After the page is persisted, the
  declared `interest_point_ids` are written back: evidence strings are appended
  as `Kind=web` entries, unprovided fields keep their original values, and
  archived/deleted points are skipped (never resurrect an obsolete memory).
  Each writeback is audited with a `reliability_update` change-log entry —
  review results override the unified adjudication's initial verdict, closing
  the trusted loop (v2 §3.5).

### Changed

- **Skipped-write audit** — points worth a wiki page that the agent loop did
  not cover are now persisted as `wiki_write_miss` change-log entries (was
  stdout-only), making misses traceable and re-runnable (v2 §3.4).
- **Identity-consistency check** — a subjective interest point folded into an
  entity/source page is flagged via an `identity_mismatch` change-log entry;
  read-only, never rewrites (v2 §8 risk 8).

## [0.2.2] - 2026-08-18

### Added

- **Cluster-grouped wikiloop** — wiki writes now group interest points into
  EBD clusters (`wiki.group_sim`, default 0.75) with one agent loop per
  cluster and one per isolated point, instead of one loop per point. Grouping
  only — never merges (merging already happened during V1.3 persist).
- **Multi-point `has_page`** — `wiki_write` now accepts `interest_point_ids`
  (array, multi-to-one) replacing the single `interest_point_id`; the system
  builds/updates one `has_page` edge per declared point id (declared only, no
  inference).
- **`ip_query` tool** — searches interest points (not wiki pages) with their
  `has_page` relationships resolved, historical scope included, keyword
  fallback when vectors are unavailable.
- **`wiki.group_sim` config** — EBD clustering threshold for wikiloop write
  grouping, wired into the writer via `SetGroupSim`.
- **Skipped-write logging** — points worth a page that the agent loop did not
  cover are logged for later backfill (v2 §3.4).

### Changed

- `reconcile` handles multi-to-one `has_page`: shared pages are enqueued once
  (visited dedupe on page id).

## [0.2.1] - 2026-08-18

### Removed

- **Legacy verify#1/#2 pipeline and interest cleaner** — removed the old
  per-candidate fact-check pipeline (`verify#1`) and independent contradiction
  stage (`verify#2`) and the standalone interest cleaner once the unified
  adjudication pipeline (V1) took over (-2516 lines).

## [0.2.0] - 2026-08-18

### Added

- **Unified adjudication pipeline (V1)** — replaces the three-stage
  `verify#1 → Clean → verify#2` flow with a single embedding-first pipeline:
  s1 dedupe-merge (string dedup + EBD clustering + per-cluster LLM merge) → s2
  EBD connected-component clustering with historical-library recall → unified
  adjudication (merge/keep/archive verdicts, contradiction detection, and
  subjective/freshness/reliability/wiki_worthy metadata in one call) → persist
  with programmatic `related` edges. `ProcessSession` now runs
  `fork → DedupeMerge → Cluster → Adjudicate → Persist → wiki`.
- **Extraction route selection (V-E)** — fork supports a `route` config
  (`prefix` / `non_prefix` / `full` / `full2`); the full+full2 route runs
  additional-pass extraction with fact-category-guided prompts.
- **Token-cache optimizations (T)** — generic in-memory LRU, content-hash
  embedding cache (model+dims keyed), short-TTL recall context cache,
  per-request entity-read dedupe, and batched usage flushing.
- **Selective wiki writes** — `wiki.selective` (default off): fork LLM judges
  each interest point's `wiki_worthy`; points judged not worth a wiki page are
  kept as interest points only.

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
[0.2.0]: https://github.com/djasdh/interest-memory/releases/tag/v0.2.0
[0.2.1]: https://github.com/djasdh/interest-memory/releases/tag/v0.2.1
[0.2.2]: https://github.com/djasdh/interest-memory/releases/tag/v0.2.2
