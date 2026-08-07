# Contributing

Thanks for considering contributing to interest-memory! This project is a
small, self-hosted memory service, and contributions that respect its design
constraints are very welcome.

## Quality over origin

Contributions are welcome whether written by a human or an AI — quality is
what counts.

## Design principles

Please read `README.md` first. A few things matter when touching code:

- **Lightweight** — keep the memory/disk footprint low; avoid adding heavy
  dependencies. New Go dependencies must be actively maintained and
  license-compatible (MIT preferred).
- **Self-host friendly** — never introduce a hard cloud dependency. LLM and
  embedding providers are already pluggable via OpenAI-compatible `base_url`.
- **Audit-first** — every structural change to memory must be recorded in
  `change_log`. New write paths must log their mutations.
- **Degrade gracefully** — optional subsystems (web search, vector index,
  cross-namespace reads) must fail soft, never block a session.

## Development setup

```bash
# Go 1.25+ with CGO (sqlite-vec static link); unit tests need no API keys
CGO_ENABLED=1 go test -race ./...

# Bridge tests (node 22+, dependency-free)
node --test bridge/opencode/memory-lib.test.mjs
node --test bridge/openclaw/interest-memory/lib.test.mjs
node --test bridge/pi/lib.test.mjs
node --test bridge/mcp-server/lib.test.mjs
node --test bridge/claudecode/hooks/lib.test.mjs
node --test bridge/codex/hooks/lib.test.mjs
node --test bridge/reasonix/hooks/lib.test.mjs

# End-to-end (requires DEEPSEEK_API_KEY + SILICONFLOW_API_KEY)
bash scripts/e2e.sh
```

## Before submitting

1. Run `gofmt -l .` (must be empty) and `CGO_ENABLED=1 go vet ./...`.
2. Run the full Go suite with `-race`.
3. If you touched a bridge, run its `node --test` suite.
4. Add a `CHANGELOG.md` entry under `[Unreleased]`.
5. If you changed behavior, consider a regression test.

## Code style

- Go code follows the standard library conventions and the existing package
  layout (`internal/<domain>`); no comments unless they explain a
  non-obvious decision.
- Bridge code keeps pure logic in a `lib` module (importable without the
  agent runtime) and wraps it in the agent-specific entry point — this keeps
  tests dependency-free.
- Do not commit `config.yaml`, `.env*`, or any API keys. Add secrets to
  `.gitignore` if you introduce a new one.

## License

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE).
