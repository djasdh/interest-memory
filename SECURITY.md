# Security Policy

## Reporting a Vulnerability

Please report security issues privately, not in public issues. Contact the
maintainer directly via GitHub (djasdh) or open a private security advisory
at https://github.com/djasdh/interest-memory/security/advisories.

We aim to acknowledge reports within 48 hours and ship a fix as soon as the
impact is understood.

## Supported versions

Security fixes are released for the latest version. Older versions are
supported only at the maintainer's discretion.

| Version | Supported |
|---|---|
| latest | :white_check_mark: |
| older | :x: |

## Threat model

interest-memory is a self-hosted, single-user or small-group memory service.
It assumes a trusted local environment and does **not** provide
authentication or authorization between consumers:

- The REST API (`/api/v1/...`) has no auth layer. Keep the server bound to
  `127.0.0.1` (the default) or a trusted network only. Do not expose it
  publicly without a reverse proxy + auth.
- API keys are read from environment variables (`api_key_env`) and never
  written to disk by the service. The interactive installer persists keys to
  `~/.config/interest-memory.env` (mode 0600) — do not commit that file.
- Database and transcripts may contain private conversation data. Protect
  the SQLite file and its WAL/shm siblings with filesystem permissions.
- `web_tool: myagent` performs live web search during verification; requests
  carry configured API keys, so verify your provider's usage policy before
  enabling it at scale.

### Secrets

- `config.yaml` and `.env*` are gitignored — never commit them.
- If you believe a key or the database file was exposed, rotate the keys and
  treat the memory contents as disclosed.
