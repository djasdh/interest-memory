#!/usr/bin/env python3
"""interest-memory LLM configurator — a script meant to be CALLED BY AN LLM.

Non-interactive, no TUI. It updates ONLY the ``llm`` section of config.yaml
(base_url / api_key_env / model); every other section is left untouched.

Running it with no arguments (or --help) prints this help text, which doubles
as the prompt an LLM agent should follow to configure the memory service's LLM.

Usage:
    python3 scripts/install_llm.py --help                     # this prompt
    python3 scripts/install_llm.py --show                     # print current llm config
    python3 scripts/install_llm.py --provider deepseek
    python3 scripts/install_llm.py --provider deepseek --model deepseek-v4-flash \\
        --key-env DEEPSEEK_API_KEY --key <key>
    python3 scripts/install_llm.py --provider custom --base-url https://host/v1 \\
        --key-env MY_KEY --model my-model
    python3 scripts/install_llm.py --dry-run --provider deepseek   # preview only
"""
from __future__ import annotations

import argparse
import os
import re
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    from install_presets import LLM_PRESETS  # noqa: E402
except ImportError:
    # 远程执行（curl | python3 -）时本地没有 presets：自动拉取到临时目录。
    import tempfile
    import urllib.request
    _tmp = tempfile.mkdtemp(prefix="im-llm-")
    _presets_url = "https://raw.githubusercontent.com/djasdh/interest-memory/main/scripts/install_presets.py"
    urllib.request.urlretrieve(_presets_url, os.path.join(_tmp, "install_presets.py"))
    sys.path.insert(0, _tmp)
    from install_presets import LLM_PRESETS  # noqa: E402

_here = Path(__file__).resolve()
REPO = _here.parent.parent if _here.is_file() else Path.cwd()  # curl | python3 - 时回退到 cwd
CONFIG = REPO / "config.yaml"
CONFIG_EXAMPLE = REPO / "config.example.yaml"
KEY_FILE = Path.home() / ".config" / "interest-memory.env"


def say(msg: str) -> None:
    print(f"\033[32m[install]\033[0m {msg}")


def warn(msg: str) -> None:
    print(f"\033[33m[warn]\033[0m {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Provider matching (fuzzy: case/space/hyphen-insensitive, substring OK)
# ---------------------------------------------------------------------------
def _norm(s: str) -> str:
    return re.sub(r"[\s_\-()]", "", s).lower()


def match_provider(q: str):
    t = _norm(q)
    for p in LLM_PRESETS:
        if _norm(p.name) == t:
            return p
    subs = [p for p in LLM_PRESETS if t in _norm(p.name)]
    if len(subs) == 1:
        return subs[0]
    return None


# ---------------------------------------------------------------------------
# config.yaml llm-section patching (mirrors install.py patch_config's llm block)
# ---------------------------------------------------------------------------
def patch_llm_section(text: str, base_url: str, key_env: str, model: str) -> str:
    out: list[str] = []
    sec = ""
    for ln in text.splitlines():
        s = ln.strip()
        if ln and not ln[0].isspace() and s and not s.startswith("#") and ":" in s:
            key = s.split(":")[0].strip()
            sec = key if key in ("llm", "embedding", "wiki") else ""
        indent = ln[: len(ln) - len(ln.lstrip())]
        if sec == "llm":
            if re.match(r"^\s*base_url:", ln):
                ln = f"{indent}base_url: {base_url}"
            elif re.match(r"^\s*api_key_env:", ln):
                ln = f"{indent}api_key_env: {key_env}"
            elif re.match(r"^\s*model:", ln):
                ln = f"{indent}model: {model}"
        out.append(ln)
    return "\n".join(out) + "\n"


def read_llm_section() -> dict[str, str]:
    if not CONFIG.exists():
        return {}
    out: dict[str, str] = {}
    sec = ""
    for ln in CONFIG.read_text(encoding="utf-8").splitlines():
        s = ln.strip()
        if ln and not ln[0].isspace() and s and not s.startswith("#") and ":" in s:
            key = s.split(":")[0].strip()
            sec = key if key == "llm" else ""
            continue
        if sec == "llm":
            m = re.match(r"^([a-z_]+):\s*(.*)$", s)
            if m:
                out[m.group(1)] = m.group(2).strip().strip("'\"")
    return out


# ---------------------------------------------------------------------------
# API key persistence (~/.config/interest-memory.env) — mirrors install.py
# ---------------------------------------------------------------------------
def load_env_file() -> dict[str, str]:
    out: dict[str, str] = {}
    try:
        for ln in KEY_FILE.read_text(encoding="utf-8").splitlines():
            ln = ln.strip()
            if not ln or ln.startswith("#") or "=" not in ln:
                continue
            k, _, v = ln.partition("=")
            out[k.strip()] = v.strip()
    except FileNotFoundError:
        pass
    return out


def save_key(key_env: str, value: str) -> None:
    KEY_FILE.parent.mkdir(parents=True, exist_ok=True)
    env = load_env_file()
    env[key_env] = value
    KEY_FILE.write_text("\n".join(f"{k}={v}" for k, v in sorted(env.items())) + "\n",
                        encoding="utf-8")
    os.environ[key_env] = value


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------
_PROMPT = """Configure the LLM used by interest-memory (fork extraction / correction fact-check).

Designed to be called by an LLM agent or a script: non-interactive, no TUI.
It updates ONLY the `llm` section of config.yaml (base_url / api_key_env / model);
every other section of config.yaml is left untouched.

Good workflow: run --show to inspect the current config, then --dry-run to
preview the change, then apply it. All steps are idempotent."""

_EPILOG = """examples:
  python3 scripts/install_llm.py --show
  python3 scripts/install_llm.py --provider deepseek
  python3 scripts/install_llm.py --provider deepseek --model deepseek-v4-flash \\
      --key-env DEEPSEEK_API_KEY --key <key>
  python3 scripts/install_llm.py --provider custom --base-url https://host/v1 \\
      --key-env MY_KEY --model my-model
  python3 scripts/install_llm.py --dry-run --provider deepseek

available providers (--provider fuzzy-matches name, e.g. 'deepseek', 'opencode-go',
'ollama', 'zhipu', 'kimi'): """ + ", ".join(p.name for p in LLM_PRESETS) + """

notes:
  - Ollama (local) is keyless: no API key is needed.
  - --key writes the key to ~/.config/interest-memory.env (the server reads keys
    from that file / the environment). Without --key only the config reference is
    updated and the key must be available in the environment at runtime.
  - If config.yaml does not exist, it is created from config.example.yaml and only
    the llm section is patched.
  - --dry-run prints the changes and makes no modifications.
  - Exit codes: 0 = ok, 1 = usage/provider error, 2 = argparse error."""


def main() -> None:
    parser = argparse.ArgumentParser(
        prog="install_llm.py",
        description=_PROMPT,
        epilog=_EPILOG,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--provider", metavar="NAME", help="provider preset name (fuzzy match)")
    parser.add_argument("--base-url", metavar="URL", help="OpenAI-compatible base URL (required only for Custom)")
    parser.add_argument("--key-env", metavar="ENV", help="env var holding the API key (default: the provider preset's)")
    parser.add_argument("--model", metavar="MODEL", help="model name (default: the provider preset's default)")
    parser.add_argument("--key", metavar="VALUE", help="API key to persist into ~/.config/interest-memory.env")
    parser.add_argument("--show", action="store_true", help="print the current llm config and exit")
    parser.add_argument("--dry-run", action="store_true", help="print changes, write nothing")
    args = parser.parse_args()

    if args.show:
        cfg = read_llm_section()
        if not cfg:
            warn(f"no llm section found (config.yaml missing: {CONFIG})")
            sys.exit(1)
        say("current llm config:")
        for k in ("base_url", "api_key_env", "model", "max_tokens"):
            if k in cfg:
                print(f"  {k}: {cfg[k]}")
        return

    if not args.provider:
        parser.print_help(sys.stderr)
        sys.exit(1)

    prov = match_provider(args.provider)
    if prov is None:
        warn(f"no provider matched {args.provider!r}. Available: "
             + ", ".join(p.name for p in LLM_PRESETS))
        sys.exit(1)

    base_url = args.base_url or prov.url
    key_env = args.key_env or prov.key_env
    model = args.model or prov.default_model
    if not base_url:
        warn("custom provider requires --base-url (OpenAI-compatible, include /v1)")
        sys.exit(1)
    if not model:
        warn("custom provider requires --model")
        sys.exit(1)
    if args.key and not key_env:
        warn(f"{prov.name} is keyless — --key is not applicable")
        sys.exit(1)

    say(f"provider: {prov.name}")
    say(f"llm.base_url: {base_url}")
    say(f"llm.api_key_env: {key_env or 'NONE'}")
    say(f"llm.model: {model}")

    source = CONFIG if CONFIG.exists() else CONFIG_EXAMPLE
    if not CONFIG.exists():
        if not CONFIG_EXAMPLE.exists():
            warn(f"config.yaml / config.example.yaml not found in {REPO} — run this inside the interest-memory repo")
            sys.exit(1)
        if args.dry_run:
            say(f"[dry-run] cp {CONFIG_EXAMPLE} {CONFIG}")
        else:
            shutil.copy(CONFIG_EXAMPLE, CONFIG)
            say(f"created {CONFIG} from example")
    text = source.read_text(encoding="utf-8")
    patched = patch_llm_section(text, base_url, key_env or "NONE", model)

    if args.dry_run:
        say(f"[dry-run] would update the llm section in {CONFIG}:")
        print(f"  base_url: {base_url}")
        print(f"  api_key_env: {key_env or 'NONE'}")
        print(f"  model: {model}")
        if args.key:
            say(f"[dry-run] would write {key_env} into {KEY_FILE}")
    else:
        CONFIG.write_text(patched, encoding="utf-8")
        say(f"updated the llm section in {CONFIG}")
        if args.key:
            save_key(key_env, args.key)
            say(f"wrote {key_env} into {KEY_FILE}")

    if args.key:
        say("key available via env var at runtime")
    elif key_env:
        if load_env_file().get(key_env):
            say(f"{key_env} is set in {KEY_FILE}")
        elif os.environ.get(key_env):
            say(f"{key_env} is set in the environment")
        else:
            warn(f"{key_env} not set — provide it via the environment at runtime "
                 f"(or rerun with --key)")
    sys.exit(0)


if __name__ == "__main__":
    main()
