#!/usr/bin/env python3
"""interest-memory installer — guided setup with a Hermes-style curses TUI.

Builds the Go server, picks LLM / embedding providers (live /models
discovery with flash-tier fallback), configures the wiki language, generates
config.yaml, installs selected agent bridges (with stale-path repair), and
optionally registers a systemd user service.

Usage:
    python3 scripts/install.py                     # guided (curses) wizard
    python3 scripts/install.py --dry-run           # print steps, change nothing
    python3 scripts/install.py --noninteractive    # defaults, no prompts
    python3 scripts/install.py --server-only       # server build + config only
    python3 scripts/install.py --systemd           # also register the systemd service
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
BIN = REPO / "bin" / "interest-memory-server"
CONFIG = REPO / "config.yaml"
CONFIG_EXAMPLE = REPO / "config.example.yaml"
OLD_REPO_PATH = "/home/umr/Projects/agent-adapt/interest-memory"
KEY_FILE = Path.home() / ".config" / "interest-memory.env"

sys.path.insert(0, str(Path(__file__).resolve().parent))
from install_curses import (  # noqa: E402
    GoBack, ask_confirm, ask_input, pick_checklist, pick_radio, pick_single,
    show_msg, with_loading,
)
from install_presets import (  # noqa: E402
    EMB_PRESETS, LLM_PRESETS, Provider, fetch_models,
)

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
def say(msg: str) -> None:
    print(f"\033[32m[install]\033[0m {msg}")


def warn(msg: str) -> None:
    print(f"\033[33m[warn]\033[0m {msg}")


def log_step(msg: str) -> None:
    print(f"\033[36m==>\033[0m \033[1m{msg}\033[0m")


# ---------------------------------------------------------------------------
# Command execution (dry-run aware)
# ---------------------------------------------------------------------------
class Ctx:
    def __init__(self, dry_run: bool, noninteractive: bool):
        self.dry_run = dry_run
        self.noninteractive = noninteractive
        self.installed: list[str] = []
        self.skipped: list[str] = []

    def run(self, *cmd, cwd=None) -> subprocess.CompletedProcess:
        """Run a command, honoring dry-run: print, and only execute when real."""
        if self.dry_run:
            where = f" (cwd={cwd})" if cwd else ""
            print("  " + " ".join(str(c) for c in cmd) + where)
            return subprocess.CompletedProcess(cmd, 0)
        return subprocess.run([str(c) for c in cmd], cwd=str(cwd) if cwd else None)


def sh(cmd: str, cwd=None, check=True) -> str:
    """Run a shell command, returning stdout."""
    r = subprocess.run(cmd, shell=True, cwd=str(cwd) if cwd else None,
                       capture_output=True, text=True)
    if check and r.returncode != 0:
        raise RuntimeError(f"{cmd}: {r.stderr.strip()}")
    return r.stdout.strip()


def which(name: str) -> bool:
    return shutil.which(name) is not None


def is_windows() -> bool:
    return os.name == "nt" or "MSYSTEM" in os.environ or "MINGW" in os.environ


# ---------------------------------------------------------------------------
# 0. Environment detection
# ---------------------------------------------------------------------------
def detect_env(ctx: Ctx) -> list[str]:
    log_step("环境检测 / Environment check")
    for tool, label in (("go", "Go"), ("node", "node"), ("npm", "npm"), ("curl", "curl")):
        if which(tool):
            say(f"{label}: OK")
        else:
            warn(f"{label}: 未找到（{tool}） / not found ({tool})")

    agents = []
    for name in ("opencode", "openclaw", "claude", "codex", "reasonix"):
        if which(name):
            agents.append(name)
    if (Path.home() / ".pi").is_dir():
        agents.append("pi")
    if (Path.home() / ".hermes").is_dir():
        agents.append("hermes")
    say("可用 agent / available agents: " + (", ".join(agents) if agents else "（无）/ (none)"))
    return agents


# ---------------------------------------------------------------------------
# 1. Server build
# ---------------------------------------------------------------------------
def build_server(ctx: Ctx) -> None:
    log_step("构建服务端 / Building server")
    if not ctx.dry_run:
        (REPO / "bin").mkdir(parents=True, exist_ok=True)
    ctx.run("go", "build", "-o", BIN, "./cmd/server", cwd=REPO)
    say(f"二进制 / binary: {BIN}")


# ---------------------------------------------------------------------------
# 2-4. Provider / language selection
# ---------------------------------------------------------------------------
def pick_provider(ctx: Ctx, presets: list[Provider], kind: str) -> Provider:
    log_step(f"选择 {kind} 供应商 / Select {kind} provider")
    labels = [p.name for p in presets]
    idx = pick_radio(f"{kind} 供应商 / {kind} provider", labels, searchable=True)
    prov = presets[idx]
    if prov.name == "Custom":
        if ctx.noninteractive:
            raise SystemExit("自定义供应商需要交互输入，--noninteractive 下不可用 / custom provider requires interactive input, unavailable under --noninteractive")
        url = ask_input(f"{kind} base_url（OpenAI 兼容，含 /v1）/ (OpenAI-compatible, include /v1)", "https://api.example.com/v1")
        key_env = ask_input("API key 环境变量名 / API key env var name", "MY_API_KEY")
        model = ask_input("默认模型名 / default model name", "my-model")
        dims = 0
        if kind == "Embedding":
            dims = int(ask_input("向量维度 / vector dimensions", "1024") or 1024)
        prov = Provider("Custom", url, key_env, model, dims=dims)
    say(f"{kind}: {prov.name}")
    return prov


def pick_model(ctx: Ctx, prov: Provider, kind: str) -> str:
    """Pick a model. Order: fetch with any available key (else default), let the
    user choose, then ask for the API key if it's still missing."""
    model = prov.default_model
    should_fetch = prov.url or prov.key_env not in (None, "CUSTOM", "")
    if should_fetch:
        if ctx.noninteractive:
            fetched = fetch_models(prov)
        else:
            # Stay inside the TUI with a loading spinner while fetching.
            # ESC/q during the fetch raises GoBack (back to the provider step).
            fetched = with_loading(f"获取 {prov.name} 模型列表 / Fetching {prov.name} models",
                                   lambda: fetch_models(prov))
    else:
        fetched = []

    if fetched:
        default_idx = 0
        if model and model in fetched:
            default_idx = fetched.index(model)
        if ctx.noninteractive:
            warn(f"--noninteractive：使用默认模型 {model}（可用 {len(fetched)} 个）/ "
                 f"using default model {model} ({len(fetched)} available)")
        else:
            chosen = pick_radio(f"{prov.name} 模型（/ 搜索）/ {prov.name} model (/ to search)", fetched, selected=default_idx,
                                searchable=True)
            model = fetched[chosen]
        say(f"{kind} 模型 / model: {model}")
    elif prov.url:
        # No live list (key missing / offline): pick from the flash default plus
        # a manual entry, THEN ask for the key.
        if not ctx.noninteractive:
            warn(f"{prov.name} 模型列表获取失败（缺 key 或网络）/ "
                 f"failed to fetch {prov.name} models (missing key or offline)")
            options = [model, "手动输入模型名 / enter model name manually"] if model else ["手动输入模型名 / enter model name manually"]
            idx = pick_radio(f"{kind} 模型（未获取到列表）/ {kind} model (no list)", options, selected=0,
                             searchable=False)
            if options[idx].startswith("手动输入"):
                model = ask_input("模型名 / model name", model or "")
            say(f"{kind} 模型 / model: {model}")
        else:
            warn(f"--noninteractive：使用默认模型 {model} / using default model {model}")
    else:
        # Ollama local or keyless: no list available at all.
        say(f"{kind} 模型 / model: {model}")

    # Ask for the key after the model is chosen.
    ensure_key(ctx, prov)
    return model


def pick_language(ctx: Ctx) -> str:
    log_step("选择 wiki 输出语言 / Select wiki output language")
    options = ["English", "中文", "Custom"]
    idx = pick_radio("wiki 输出语言 / wiki output language", options, selected=0, searchable=False)
    lang = options[idx]
    if lang == "Custom":
        if ctx.noninteractive:
            lang = "English"
        else:
            lang = ask_input("语言（写入 wiki.language）/ language (written to wiki.language)", "English")
    say(f"wiki.language: {lang}")
    return lang


# ---------------------------------------------------------------------------
# 5. config.yaml
# ---------------------------------------------------------------------------
def patch_config(llm: Provider, llm_model: str, emb: Provider, emb_model: str, lang: str) -> None:
    """In-place update of llm/embedding/wiki.language blocks (preserve indent)."""
    f = CONFIG
    text = f.read_text(encoding="utf-8")
    lines = text.splitlines()
    out: list[str] = []
    sec = ""

    def sub(indent: str, key: str, val: str) -> str:
        return f"{indent}{key}: {val}"

    for ln in lines:
        s = ln.strip()
        if ln and not ln[0].isspace() and s and not s.startswith("#") and ":" in s:
            key = s.split(":")[0].strip()
            sec = key if key in ("llm", "embedding", "wiki") else ""
        indent = ln[: len(ln) - len(ln.lstrip())]
        m = re.match(r"^\s*base_url:", ln)
        if sec == "llm":
            if m: ln = sub(indent, "base_url", llm.url)
            elif re.match(r"^\s*api_key_env:", ln): ln = sub(indent, "api_key_env", llm.key_env or "NONE")
            elif re.match(r"^\s*model:", ln): ln = sub(indent, "model", llm_model)
        elif sec == "embedding":
            if m: ln = sub(indent, "base_url", emb.url)
            elif re.match(r"^\s*api_key_env:", ln): ln = sub(indent, "api_key_env", emb.key_env or "NONE")
            elif re.match(r"^\s*model:", ln): ln = sub(indent, "model", emb_model)
            elif re.match(r"^\s*dimensions:", ln): ln = sub(indent, "dimensions", str(emb.dims or 1024))
        elif sec == "wiki":
            if re.match(r"^\s*language:", ln): ln = sub(indent, "language", lang)
        out.append(ln)
    f.write_text("\n".join(out) + "\n", encoding="utf-8")


def write_config(ctx: Ctx, llm: Provider, llm_model: str, emb: Provider, emb_model: str, lang: str) -> None:
    log_step("生成 config.yaml / Generating config.yaml")
    if CONFIG.exists():
        warn(f"{CONFIG} 已存在 / already exists")
        if ctx.noninteractive or ask_confirm(f"检测到已有 {CONFIG}。是否将 llm/embedding/wiki.language 更新为本次选择？（其余段保留）"
                                             f" / {CONFIG} exists. Update llm/embedding/wiki.language to this selection? (other sections kept)"):
            if ctx.dry_run:
                print(f"  patch llm/embedding/wiki.language in {CONFIG}")
            else:
                patch_config(llm, llm_model, emb, emb_model, lang)
        else:
            say("保留现有 config.yaml 不动 / keeping existing config.yaml")
        return
    if ctx.dry_run:
        print(f"  cp {CONFIG_EXAMPLE} {CONFIG}")
        print(f"  patch llm/embedding/wiki.language in {CONFIG}")
    else:
        shutil.copy(CONFIG_EXAMPLE, CONFIG)
        patch_config(llm, llm_model, emb, emb_model, lang)
    say(f"已生成 / generated {CONFIG}")


# ---------------------------------------------------------------------------
# 6-7. Bridge installation
# ---------------------------------------------------------------------------
def _confirm(ctx: Ctx, text: str) -> bool:
    if ctx.noninteractive:
        return True
    return ask_confirm(text)


def install_opencode(ctx: Ctx) -> None:
    dst = Path.home() / ".config" / "opencode" / "plugin"
    log_step(f"安装 opencode bridge → {dst} / Installing opencode bridge → {dst}")
    if _confirm(ctx, f"安装 opencode bridge 到 {dst} ? / Install opencode bridge to {dst}?"):
        ctx.run("mkdir", "-p", dst)
        ctx.run("cp", REPO / "bridge/opencode/memory.ts", dst / "memory.ts")
        ctx.run("cp", REPO / "bridge/opencode/memory-lib.ts", dst / "memory-lib.ts")
        ctx.installed.append("opencode")
    else:
        ctx.skipped.append("opencode")


def install_pi(ctx: Ctx) -> None:
    dst = Path.home() / ".pi" / "agent" / "extensions" / "interest-memory"
    log_step(f"安装 pi bridge → {dst} / Installing pi bridge → {dst}")
    if _confirm(ctx, f"安装 pi bridge 到 {dst} ? / Install pi bridge to {dst}?"):
        ctx.run("mkdir", "-p", dst)
        ctx.run("cp", REPO / "bridge/pi/memory.ts", dst / "index.ts")
        ctx.run("cp", REPO / "bridge/pi/lib.ts", dst / "lib.ts")
        ctx.installed.append("pi")
    else:
        ctx.skipped.append("pi")


def install_openclaw(ctx: Ctx) -> None:
    dst = Path.home() / ".openclaw" / "extensions" / "interest-memory"
    log_step(f"安装 openclaw bridge → {dst} / Installing openclaw bridge → {dst}")
    if _confirm(ctx, f"安装 openclaw bridge 到 {dst} ?（含 npm install）/ Install openclaw bridge to {dst}? (includes npm install)"):
        if not ctx.dry_run:
            shutil.rmtree(dst, ignore_errors=True)
        else:
            print(f"  rm -rf {dst}")
        ctx.run("cp", "-r", REPO / "bridge/openclaw/interest-memory", dst)
        if ctx.dry_run:
            print(f"  (cd {dst} && npm install --no-audit --no-fund)")
        else:
            subprocess.run("npm install --no-audit --no-fund", shell=True, cwd=str(dst),
                           check=False)
        ctx.installed.append("openclaw")
    else:
        ctx.skipped.append("openclaw")


def install_hermes(ctx: Ctx) -> None:
    base = Path(os.environ.get("HERMES_HOME") or Path.home() / ".hermes")
    dst = base / "plugins" / "interest"
    log_step(f"安装 hermes bridge → {dst} / Installing hermes bridge → {dst}")
    if _confirm(ctx, f"安装 hermes bridge 到 {dst} ? / Install hermes bridge to {dst}?"):
        ctx.run("mkdir", "-p", dst)
        ctx.run("cp", "-r", str(REPO / "bridge/hermes/.") + "/", dst)
        warn("hermes 需在 config.yaml 设置 memory.provider: interest / hermes needs memory.provider: interest in config.yaml")
        ctx.installed.append("hermes")
    else:
        ctx.skipped.append("hermes")


def install_claudecode(ctx: Ctx) -> None:
    dst = Path.home() / ".claude" / "plugins" / "interest-memory"
    log_step(f"安装 claudecode bridge → {dst} / Installing claudecode bridge → {dst}")
    if _confirm(ctx, f"安装 claudecode bridge 到 {dst} ?（MCP 路径替换为 {REPO}）/ Install claudecode bridge to {dst}? (MCP path set to {REPO})"):
        if not ctx.dry_run:
            shutil.rmtree(dst, ignore_errors=True)
        else:
            print(f"  rm -rf {dst}")
        ctx.run("cp", "-r", REPO / "bridge/claudecode", dst)
        if ctx.dry_run:
            print(f"  sed -i s#__INTEREST_REPO__#{REPO}#g {dst}/.mcp.json")
        else:
            mcp = dst / ".mcp.json"
            mcp.write_text(mcp.read_text(encoding="utf-8").replace("__INTEREST_REPO__", str(REPO)),
                           encoding="utf-8")
        warn(f"使用方式 / usage: claude --plugin-dir {dst}")
        ctx.installed.append("claudecode")
    else:
        ctx.skipped.append("claudecode")


def install_codex(ctx: Ctx) -> None:
    log_step("安装/修复 codex bridge / Installing/repairing codex bridge")
    hooks = Path.home() / ".codex" / "hooks.json"
    cfg = Path.home() / ".codex" / "config.toml"
    if _confirm(ctx, "安装/修复 codex bridge（hooks.json + config.toml MCP）? / Install/repair codex bridge (hooks.json + config.toml MCP)?"):
        ctx.run("mkdir", "-p", str(Path.home() / ".codex"))
        if ctx.dry_run:
            print(f"  write hooks.json referencing {REPO}/bridge/codex/hooks/*.mjs")
        else:
            hooks.parent.mkdir(parents=True, exist_ok=True)
            hooks.write_text(json.dumps({
                "hooks": {
                    "UserPromptSubmit": [{"hooks": [{"type": "command",
                        "command": f'"{REPO}/bridge/codex/hooks/recall.mjs"'}]}],
                    "SessionEnd": [{"hooks": [{"type": "command",
                        "command": f'"{REPO}/bridge/codex/hooks/ingest.mjs"', "timeout": 3}]}],
                }
            }, indent=2) + "\n", encoding="utf-8")
        if ctx.dry_run:
            print(f"  update/add [mcp_servers.interest-memory] in {cfg}")
        else:
            block = (f"\n[mcp_servers.interest-memory]\ncommand = \"node\"\n"
                     f"args = [\"{REPO}/bridge/mcp-server/server.ts\"]\n"
                     f"env = {{ INTEREST_AGENT = \"codex\" }}\n")
            if cfg.exists() and "mcp_servers.interest-memory" in cfg.read_text(encoding="utf-8"):
                say("codex MCP 已存在，检查路径... / codex MCP exists, checking path...")
                t = cfg.read_text(encoding="utf-8").replace(OLD_REPO_PATH, str(REPO))
                cfg.write_text(t, encoding="utf-8")
            else:
                with cfg.open("a", encoding="utf-8") as f:
                    f.write(block)
        ctx.installed.append("codex")
    else:
        ctx.skipped.append("codex")


def install_reasonix(ctx: Ctx) -> None:
    log_step("安装 reasonix bridge / Installing reasonix bridge")
    dst = Path.home() / ".local" / "share" / "interest-memory" / "reasonix-plugin"
    if _confirm(ctx, "安装 reasonix bridge（副本 + 展开 MCP 路径 + --link）? / Install reasonix bridge (copy + expand MCP path + --link)?"):
        if not ctx.dry_run:
            shutil.rmtree(dst, ignore_errors=True)
            dst.parent.mkdir(parents=True, exist_ok=True)
        else:
            print(f"  rm -rf {dst}; mkdir -p {dst.parent}")
        ctx.run("cp", "-r", REPO / "bridge/reasonix", dst)
        if ctx.dry_run:
            print(f"  sed -i s#__INTEREST_REPO__#{REPO}#g {dst}/reasonix-plugin.json")
            print(f"  reasonix plugin install {dst} --link --replace --yes")
        else:
            manifest = dst / "reasonix-plugin.json"
            manifest.write_text(manifest.read_text(encoding="utf-8").replace("__INTEREST_REPO__", str(REPO)),
                                encoding="utf-8")
            if which("reasonix"):
                subprocess.run("reasonix plugin remove interest-memory --yes", shell=True,
                               capture_output=True)
                subprocess.run(f'reasonix plugin install "{dst}" --link --replace --yes',
                               shell=True, check=False)
            else:
                warn(f"未找到 reasonix 命令，已复制到 {dst}，请手动安装 / "
                     f"reasonix command not found; copied to {dst}, please install manually")
        ctx.installed.append("reasonix")
    else:
        ctx.skipped.append("reasonix")


BRIDGE_INSTALLERS = {
    "opencode": install_opencode,
    "pi": install_pi,
    "openclaw": install_openclaw,
    "hermes": install_hermes,
    "claudecode": install_claudecode,
    "codex": install_codex,
    "reasonix": install_reasonix,
}


def pick_bridges(ctx: Ctx, agents: list[str]) -> list[str]:
    log_step("选择要安装的 bridge / Select bridges to install")
    order = ["opencode", "pi", "openclaw", "hermes", "claudecode", "codex", "reasonix"]
    available = [a for a in order if a in agents]
    if not available:
        warn("没有检测到任何可安装的 agent，跳过 bridge 安装 / no installable agent detected, skipping bridge install")
        return []
    labels = [f"{a}: {BRIDGE_DESC[a]}" for a in available]
    if ctx.noninteractive:
        chosen = available
        say("非交互模式：安装全部可用 bridge / noninteractive: installing all available bridges: " + " ".join(chosen))
    else:
        idxs = pick_checklist("选择要安装的 bridge / Select bridges to install", labels,
                              selected=set(range(len(available))))
        chosen = [available[i] for i in idxs]
        say("将安装 bridge / bridges to install: " + (" ".join(chosen) if chosen else "（无）/ (none)"))
    return chosen


BRIDGE_DESC = {
    "opencode": "~/.config/opencode/plugin",
    "pi": "~/.pi/agent/extensions",
    "openclaw": "~/.openclaw/extensions",
    "hermes": "$HERMES_HOME/plugins/interest",
    "claudecode": "~/.claude/plugins/interest-memory",
    "codex": "~/.codex",
    "reasonix": "~/.reasonix/plugins",
}


# ---------------------------------------------------------------------------
# systemd user service
# ---------------------------------------------------------------------------
def install_systemd(ctx: Ctx) -> None:
    log_step("注册 systemd 用户服务 / Registering systemd user service")
    if is_windows():
        warn("Windows 不支持 systemd，跳过（可用手动方式常驻）/ Windows has no systemd; skipping (run it manually)")
        ctx.skipped.append("systemd")
        return
    if ctx.noninteractive or ask_confirm("注册 systemd 用户服务（开机自启 interest-memory）? / Register systemd user service (autostart interest-memory)?"):
        unit_dir = Path.home() / ".config" / "systemd" / "user"
        unit = unit_dir / "interest-memory.service"
        env_file = Path.home() / ".config" / "interest-memory.env"
        if ctx.dry_run:
            print(f"  mkdir -p {unit_dir}")
            print(f"  write {unit}")
            print("  systemctl --user daemon-reload && enable --now interest-memory.service")
        else:
            unit_dir.mkdir(parents=True, exist_ok=True)
            db_path = _config_db_path()
            db_path = str(db_path).replace("~", str(Path.home()))
            if not db_path.startswith("/"):
                db_path = str(Path.home() / db_path)
            Path(db_path).parent.mkdir(parents=True, exist_ok=True)
            unit.write_text(
                f"[Unit]\nDescription=interest-memory (agent long-term memory service)\n"
                f"After=network-online.target\nWants=network-online.target\n\n"
                f"[Service]\nType=simple\nExecStart={BIN} -config {CONFIG}\n"
                f"Restart=on-failure\nRestartSec=5\nEnvironmentFile=-{env_file}\n\n"
                f"[Install]\nWantedBy=default.target\n",
                encoding="utf-8")
            subprocess.run("systemctl --user daemon-reload", shell=True, check=False)
            subprocess.run("systemctl --user enable --now interest-memory.service",
                           shell=True, check=False)
            say("已启用并启动 interest-memory.service / enabled and started interest-memory.service")
        ctx.installed.append("systemd")
    else:
        ctx.skipped.append("systemd")


def _config_db_path() -> str:
    try:
        for ln in CONFIG.read_text(encoding="utf-8").splitlines():
            m = re.match(r"^\s*db_path:\s*(.+)$", ln)
            if m:
                return m.group(1).strip().strip("'\"")
    except FileNotFoundError:
        pass
    return "~/.interest-memory/memory.db"


# ---------------------------------------------------------------------------
# API key management (~/.config/interest-memory.env)
# ---------------------------------------------------------------------------
def load_env_file() -> dict[str, str]:
    """Load KEY=value pairs from ~/.config/interest-memory.env."""
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
    """Persist one key into ~/.config/interest-memory.env (preserving others)."""
    KEY_FILE.parent.mkdir(parents=True, exist_ok=True)
    env = load_env_file()
    env[key_env] = value
    lines = [f"{k}={v}" for k, v in sorted(env.items())]
    KEY_FILE.write_text("\n".join(lines) + "\n", encoding="utf-8")
    os.environ[key_env] = value  # make it visible to this run and children
    say(f"已写入 {KEY_FILE} ({key_env}) / wrote {KEY_FILE} ({key_env})")


def ensure_key(ctx: Ctx, prov: Provider) -> None:
    """If the provider's key env var is unset (and not keyless/custom), ask to
    set it now via the TUI and persist it to the env file."""
    if prov.key_env in (None, "", "CUSTOM"):
        return
    if os.environ.get(prov.key_env):
        return
    if ctx.noninteractive:
        warn(f"{prov.key_env} 未设置（--noninteractive 跳过询问）/ "
             f"{prov.key_env} not set (--noninteractive skips asking)")
        return
    if not ask_confirm(f"{prov.name} 需要 {prov.key_env}。是否现在设置？/ "
                       f"{prov.name} needs {prov.key_env}. Set it now?", default=False):
        warn(f"{prov.key_env} 未设置，使用默认模型 / {prov.key_env} not set, using default model")
        return
    key = ask_input(f"请输入 {prov.key_env}（写入 {KEY_FILE}）/ enter {prov.key_env} (written to {KEY_FILE})", "")
    if key:
        save_key(prov.key_env, key)
    else:
        warn(f"未输入 {prov.key_env}，使用默认模型 / no {prov.key_env} entered, using default model")


# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
def verify(ctx: Ctx) -> None:
    log_step("验证 / Verification")
    if ctx.dry_run:
        return
    try:
        sh("go test ./...", cwd=REPO)
        say("go test 通过 / go test passed")
    except RuntimeError:
        warn("go test 失败（可忽略）/ go test failed (ignorable)")
    bridge_tests = {
        "opencode": "bridge/opencode/memory-lib.test.mjs",
        "pi": "bridge/pi/lib.test.mjs",
        "openclaw": "bridge/openclaw/interest-memory/lib.test.mjs",
        "claudecode": "bridge/claudecode/hooks/lib.test.mjs",
        "codex": "bridge/codex/hooks/lib.test.mjs",
        "reasonix": "bridge/reasonix/hooks/lib.test.mjs",
    }
    for name in ctx.installed:
        rel = bridge_tests.get(name)
        if not rel:
            continue
        try:
            sh(f"node --test {rel}", cwd=REPO)
            say(f"{name} lib 测试通过 / {name} lib tests passed")
        except RuntimeError:
            warn(f"{name} 测试失败 / {name} tests failed")


# ---------------------------------------------------------------------------
# Wizard steps (navigable: ESC/q goes back one step, Ctrl+C exits cleanly)
# ---------------------------------------------------------------------------
def _step_llm_provider(ctx: Ctx, st: dict) -> None:
    st["llm"] = pick_provider(ctx, LLM_PRESETS, "LLM")


def _step_llm_model(ctx: Ctx, st: dict) -> None:
    st["llm_model"] = pick_model(ctx, st["llm"], "LLM")


def _step_emb_provider(ctx: Ctx, st: dict) -> None:
    st["emb"] = pick_provider(ctx, EMB_PRESETS, "Embedding")


def _step_emb_model(ctx: Ctx, st: dict) -> None:
    st["emb_model"] = pick_model(ctx, st["emb"], "Embedding")


def _step_language(ctx: Ctx, st: dict) -> None:
    st["lang"] = pick_language(ctx)


def _step_config(ctx: Ctx, st: dict) -> None:
    write_config(ctx, st["llm"], st["llm_model"], st["emb"], st["emb_model"], st["lang"])


def _step_bridges(ctx: Ctx, st: dict, steps: list[str]) -> None:
    st["chosen"] = pick_bridges(ctx, st["agents"])
    pos = steps.index("bridges")
    steps[pos + 1:pos + 1] = [f"bridge:{name}" for name in st["chosen"]]


def _step_bridge(ctx: Ctx, st: dict, name: str) -> None:
    BRIDGE_INSTALLERS[name](ctx)


def _step_systemd(ctx: Ctx, st: dict) -> None:
    install_systemd(ctx)


def _step_done(ctx: Ctx, st: dict, args) -> None:
    if args.server_only:
        say("完成（server-only）/ done (server-only)")
        return
    verify(ctx)
    log_step("汇总 / Summary")
    say(f"服务端二进制 / server binary: {BIN}")
    say(f"配置文件 / config file: {CONFIG}" if CONFIG.exists() else warn(f"未生成 / not generated {CONFIG}"))
    print(f"已安装 / installed: {', '.join(ctx.installed) or '（无）/ (none)'}")
    print(f"跳过 / skipped:   {', '.join(ctx.skipped) or '（无）/ (none)'}")
    if args.dry_run:
        warn("dry-run：以上为将执行的步骤，未做任何更改 / dry-run: steps above were printed, nothing changed")
    else:
        show_msg("完成 / Done", [f"启动服务端 / start the server:", f"  {BIN} -config {CONFIG}",
                                 "", "（API key 请通过环境变量提供，如 OPENCODE_API_KEY）/ (provide API keys via env vars, e.g. OPENCODE_API_KEY)"])


def _run_step(step: str, ctx: Ctx, st: dict, args, steps: list[str]) -> None:
    if step == "llm_provider":
        _step_llm_provider(ctx, st)
    elif step == "llm_model":
        _step_llm_model(ctx, st)
    elif step == "emb_provider":
        _step_emb_provider(ctx, st)
    elif step == "emb_model":
        _step_emb_model(ctx, st)
    elif step == "language":
        _step_language(ctx, st)
    elif step == "config":
        _step_config(ctx, st)
    elif step == "bridges":
        _step_bridges(ctx, st, steps)
    elif step == "systemd":
        _step_systemd(ctx, st)
    elif step == "done":
        _step_done(ctx, st, args)
    elif step.startswith("bridge:"):
        _step_bridge(ctx, st, step[len("bridge:"):])


def _confirm_exit(ctx: Ctx) -> bool:
    """Confirm quitting when changes were already made (ESC/q at the first step)."""
    if not bool(ctx.installed) and not CONFIG.exists():
        return True
    try:
        return ask_confirm("已进行部分更改（config/bridge）。确定退出？已做的更改会保留。"
                           " / Some changes were made (config/bridges). Quit? Changes already made will be kept.")
    except GoBack:
        return False


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
def main() -> None:
    try:
        _run()
    except KeyboardInterrupt:
        print()
        warn("已取消（Ctrl+C）/ cancelled (Ctrl+C)")
        sys.exit(130)


def _run() -> None:
    ap = argparse.ArgumentParser(description="interest-memory 安装向导 / installer wizard")
    ap.add_argument("--dry-run", action="store_true", help="只打印步骤，不执行 / print steps only, no changes")
    ap.add_argument("--noninteractive", action="store_true", help="全默认，无提示 / use defaults, no prompts")
    ap.add_argument("--server-only", action="store_true", help="只做服务端构建+配置 / server build + config only")
    ap.add_argument("--systemd", action="store_true", help="同时注册 systemd 服务 / also register systemd service")
    ap.add_argument("--no-deps", action="store_true", help="跳过依赖检查（供 install.sh 使用）/ skip dep checks (for install.sh)")
    args = ap.parse_args()

    ctx = Ctx(args.dry_run, args.noninteractive)
    st: dict = {
        "agents": [],
        "llm": None, "llm_model": "",
        "emb": None, "emb_model": "",
        "lang": "",
        "chosen": [],
    }

    st["agents"] = detect_env(ctx)
    build_server(ctx)

    show_msg("interest-memory 安装向导 / installer wizard",
             ["将安装 interest-memory 记忆服务及 agent 插件。 / Will install the interest-memory service and agent bridges.",
              "所有步骤幂等，可重复执行。 / All steps are idempotent and rerunnable. "
              "Enter 确认继续，ESC/q 返回上一步，Ctrl+C 退出。 / Enter confirms, ESC/q steps back, Ctrl+C quits."])

    steps = ["llm_provider", "llm_model",
             "emb_provider", "emb_model", "language", "config"]
    if args.server_only:
        if args.systemd:
            steps.append("systemd")
    else:
        steps.append("bridges")
        if args.systemd:
            steps.append("systemd")
    steps.append("done")

    i = 0
    while 0 <= i < len(steps):
        step = steps[i]
        try:
            _run_step(step, ctx, st, args, steps)
        except GoBack:
            if i == 0:
                if _confirm_exit(ctx):
                    say("已退出 / exited")
                    return
                continue  # stay on the first step
            i -= 1
            continue
        if step == "done":
            break
        i += 1


if __name__ == "__main__":
    main()
