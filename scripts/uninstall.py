#!/usr/bin/env python3
"""interest-memory 卸载器 / uninstaller — TUI 勾选卸载。

镜像 install.py 的用户级安装产物，逐项可勾选卸载：
  - 服务端（bin/ + config.yaml）、API key、systemd 用户服务
  - 各 agent bridge（opencode / pi / openclaw / hermes / claudecode / codex / reasonix）
  - 数据目录（默认不勾选 —— 不勾选即保留，不弹"是否删除"确认）

说明：
  - codex config.toml 中追加的 [mcp_servers.interest-memory] 注入块会被精确移除（保留其余）。
  - codex hooks.json 在安装时被整体覆盖且无备份，卸载只提示不处理。

用法：
    python3 scripts/uninstall.py                     # TUI 勾选卸载
    python3 scripts/uninstall.py --dry-run           # 打印将卸载内容，不执行
    python3 scripts/uninstall.py --noninteractive    # 默认勾选（不含数据目录）直接卸载
"""
from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
BIN = REPO / "bin" / "interest-memory-server"
CONFIG = REPO / "config.yaml"
KEY_FILE = Path.home() / ".config" / "interest-memory.env"
SYSTEMD_UNIT = Path.home() / ".config" / "systemd" / "user" / "interest-memory.service"
DEFAULT_DB_DIR = Path.home() / ".interest-memory"
DSH_PACKAGE = os.environ.get("INTEREST_DSH_PACKAGE", "@djasdh/interest-memory-dsh-bridge")

sys.path.insert(0, str(Path(__file__).resolve().parent))
from install_curses import (  # noqa: E402
    GoBack, ask_confirm, pick_checklist,
)


def say(msg: str) -> None:
    print(f"\033[32m[卸载]\033[0m {msg}")


def warn(msg: str) -> None:
    print(f"\033[33m[警告]\033[0m {msg}", file=sys.stderr)


def log_step(msg: str) -> None:
    print(f"\033[36m==>\033[0m \033[1m{msg}\033[0m")


def which(name: str) -> bool:
    return shutil.which(name) is not None


# ---------------------------------------------------------------------------
# 删除辅助
# ---------------------------------------------------------------------------
def _rm(p: Path) -> bool:
    if p.is_symlink() or p.is_file():
        try:
            p.unlink()
            return True
        except OSError:
            return False
    if p.is_dir():
        shutil.rmtree(p, ignore_errors=True)
        return True
    return False


def _preview(path: Path) -> None:
    print(f"  删除 / remove: {path}")


# ---------------------------------------------------------------------------
# 各组件探测与删除
# ---------------------------------------------------------------------------
def _has_server() -> bool:
    return BIN.exists() or CONFIG.exists()


def _del_server(dry: bool) -> bool:
    if dry:
        _preview(BIN)
        _preview(CONFIG)
        return True
    ok = _rm(BIN)
    ok = _rm(CONFIG) or ok
    say(f"已删除 / removed: {BIN} {CONFIG}")
    return ok


def _has_env() -> bool:
    return KEY_FILE.exists()


def _del_env(dry: bool) -> bool:
    if dry:
        _preview(KEY_FILE)
        return True
    ok = _rm(KEY_FILE)
    say(f"已删除 / removed: {KEY_FILE}")
    return ok


def _has_systemd() -> bool:
    return SYSTEMD_UNIT.exists()


def _del_systemd(dry: bool) -> bool:
    if dry:
        print("  systemctl --user disable --now interest-memory.service")
        print("  systemctl --user daemon-reload")
        _preview(SYSTEMD_UNIT)
        return True
    if which("systemctl"):
        subprocess.run(["systemctl", "--user", "disable", "--now", "interest-memory.service"],
                       capture_output=True, check=False)
        subprocess.run(["systemctl", "--user", "daemon-reload"], capture_output=True, check=False)
    ok = _rm(SYSTEMD_UNIT)
    say(f"已删除 / removed: {SYSTEMD_UNIT}")
    return ok


# ---- 各 bridge --------------------------------------------------------------
OPENCODE_FILES = [
    Path.home() / ".config" / "opencode" / "plugin" / "memory.ts",
    Path.home() / ".config" / "opencode" / "plugin" / "memory-lib.ts",
]
PI_DIR = Path.home() / ".pi" / "agent" / "extensions" / "interest-memory"
OPENCLAW_DIR = Path.home() / ".openclaw" / "extensions" / "interest-memory"
HERMES_DIR = Path(os.environ.get("HERMES_HOME") or str(Path.home() / ".hermes")) / "plugins" / "interest"
CLAUDE_DIR = Path.home() / ".claude" / "plugins" / "interest-memory"
CODEX_CFG = Path.home() / ".codex" / "config.toml"
CODEX_HOOKS = Path.home() / ".codex" / "hooks.json"
REASONIX_DIR = Path.home() / ".local" / "share" / "interest-memory" / "reasonix-plugin"


def _has_opencode() -> bool:
    return any(p.exists() for p in OPENCODE_FILES)


def _del_opencode(dry: bool) -> bool:
    if dry:
        for p in OPENCODE_FILES:
            _preview(p)
        return True
    ok = True
    for p in OPENCODE_FILES:
        ok = _rm(p) and ok
    say(f"已删除 / removed: {OPENCODE_FILES[0].parent}")
    return ok


def _has_pi() -> bool:
    return PI_DIR.exists() or PI_DIR.is_symlink()


def _del_pi(dry: bool) -> bool:
    if dry:
        _preview(PI_DIR)
        return True
    ok = _rm(PI_DIR)
    say(f"已删除 / removed: {PI_DIR}")
    return ok


def _has_openclaw() -> bool:
    return OPENCLAW_DIR.exists() or OPENCLAW_DIR.is_symlink()


def _del_openclaw(dry: bool) -> bool:
    if dry:
        _preview(OPENCLAW_DIR)
        return True
    ok = _rm(OPENCLAW_DIR)
    say(f"已删除 / removed: {OPENCLAW_DIR}")
    return ok


def _has_hermes() -> bool:
    return HERMES_DIR.exists() or HERMES_DIR.is_symlink()


def _del_hermes(dry: bool) -> bool:
    if dry:
        _preview(HERMES_DIR)
        return True
    ok = _rm(HERMES_DIR)
    say(f"已删除 / removed: {HERMES_DIR}")
    return ok


def _has_claudecode() -> bool:
    return CLAUDE_DIR.exists() or CLAUDE_DIR.is_symlink()


def _del_claudecode(dry: bool) -> bool:
    if dry:
        _preview(CLAUDE_DIR)
        return True
    ok = _rm(CLAUDE_DIR)
    say(f"已删除 / removed: {CLAUDE_DIR}")
    return ok


def _has_codex() -> bool:
    return CODEX_CFG.exists() and "mcp_servers.interest-memory" in CODEX_CFG.read_text(encoding="utf-8")


def _del_codex(dry: bool) -> bool:
    if dry:
        print(f"  移除 [mcp_servers.interest-memory] 注入块（保留其余）/ remove injected block from {CODEX_CFG}")
        return True
    if not CODEX_CFG.exists():
        return False
    t = CODEX_CFG.read_text(encoding="utf-8")
    new = re.sub(r"(?m)^\[mcp_servers\.interest-memory\].*?(?=^\[|\Z)", "", t, flags=re.DOTALL)
    new = re.sub(r"\n{3,}", "\n\n", new)
    new = new.strip() + "\n" if new.strip() else ""
    CODEX_CFG.write_text(new, encoding="utf-8")
    say(f"已移除 / removed: [mcp_servers.interest-memory] from {CODEX_CFG}")
    return True


def _has_reasonix() -> bool:
    return REASONIX_DIR.exists() or REASONIX_DIR.is_symlink()


def _del_reasonix(dry: bool) -> bool:
    if dry:
        print("  reasonix plugin remove interest-memory --yes")
        _preview(REASONIX_DIR)
        return True
    if which("reasonix"):
        subprocess.run("reasonix plugin remove interest-memory --yes", shell=True,
                       capture_output=True, check=False)
    ok = _rm(REASONIX_DIR)
    say(f"已删除 / removed: {REASONIX_DIR}")
    return ok


# ---- dsh bridge（由 `dsh plugin --profile <p> add|remove <pkg>` 管理，
#       卸载/更新需手动执行，脚本只检测并提示）---------------------------------
def _dsh_installed_profiles() -> list[Path]:
    profiles = Path.home() / ".dsh" / "profiles"
    if not profiles.is_dir():
        return []
    installed = []
    for p in sorted(profiles.iterdir()):
        if not p.is_dir():
            continue
        if (p / "node_modules" / DSH_PACKAGE).exists():
            installed.append(p)
    return installed


def _has_dsh() -> bool:
    return len(_dsh_installed_profiles()) > 0


def _del_dsh(dry: bool) -> bool:
    log_step("dsh bridge 需手动卸载 / dsh bridge uninstall is manual")
    for prof in _dsh_installed_profiles():
        print(f"  1. dsh plugin --profile {prof.name} remove {DSH_PACKAGE}")
        print(f"  2. 移除 {prof / 'cordis.patch.yml'} 中的 interest-memory 挂载行（若存在），然后重启 DSH")
    return True


# ---- 数据目录（默认保留）----------------------------------------------------
def _configured_db_path() -> Path:
    try:
        for ln in CONFIG.read_text(encoding="utf-8").splitlines():
            m = re.match(r"^\s*db_path:\s*(.+)$", ln)
            if m:
                v = m.group(1).strip().strip("'\"")
                v = v.replace("~", str(Path.home()))
                p = Path(v).expanduser()
                return p if p.is_absolute() else Path.home() / p
    except FileNotFoundError:
        pass
    return DEFAULT_DB_DIR / "memory.db"


def _has_data() -> bool:
    return _configured_db_path().exists() or _configured_db_path().parent.exists()


def _del_data(dry: bool) -> bool:
    db = _configured_db_path()
    parent = db.parent
    if dry:
        _preview(db)
        if parent.name == ".interest-memory":
            _preview(parent)
        return True
    ok = _rm(db)
    # 仅当父目录是默认数据目录时才整体删除；其他路径只删 db 文件，避免误删用户其他数据。
    if parent.name == ".interest-memory":
        ok = _rm(parent) or ok
    say(f"已删除 / removed: {parent}")
    return ok


# ---------------------------------------------------------------------------
# 组件注册
# ---------------------------------------------------------------------------
def components() -> list[dict]:
    return [
        {"id": "server", "label": f"服务端 server ({BIN}, {CONFIG})",
         "present": _has_server, "remove": _del_server},
        {"id": "env", "label": f"API key 文件 ({KEY_FILE})",
         "present": _has_env, "remove": _del_env},
        {"id": "systemd", "label": "systemd 用户服务 (interest-memory.service)",
         "present": _has_systemd, "remove": _del_systemd},
        {"id": "opencode", "label": "opencode bridge",
         "present": _has_opencode, "remove": _del_opencode},
        {"id": "pi", "label": "pi bridge",
         "present": _has_pi, "remove": _del_pi},
        {"id": "openclaw", "label": "openclaw bridge",
         "present": _has_openclaw, "remove": _del_openclaw},
        {"id": "hermes", "label": "hermes bridge",
         "present": _has_hermes, "remove": _del_hermes},
        {"id": "claudecode", "label": "claudecode bridge",
         "present": _has_claudecode, "remove": _del_claudecode},
        {"id": "codex", "label": "codex bridge（config.toml 注入块）",
         "present": _has_codex, "remove": _del_codex},
        {"id": "reasonix", "label": "reasonix bridge",
         "present": _has_reasonix, "remove": _del_reasonix},
        {"id": "dsh", "label": "dsh bridge（手动卸载）",
         "present": _has_dsh, "remove": _del_dsh},
        {"id": "data", "label": f"数据目录 data ({_configured_db_path().parent})",
         "present": _has_data, "remove": _del_data},
    ]


def _hooks_hint() -> None:
    if not CODEX_HOOKS.exists():
        return
    warn("codex 的 hooks.json 在安装时被整体写入且无备份，本次未做修改。")
    warn(f"若不再使用 interest-memory，请手动编辑 {CODEX_HOOKS} 移除相关 hooks，或删除整个文件。")


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------
def _run() -> None:
    ap = argparse.ArgumentParser(description="interest-memory 卸载器 / uninstaller")
    ap.add_argument("--dry-run", action="store_true",
                    help="演练：只打印将卸载的内容，不执行 / print steps only, no changes")
    ap.add_argument("--noninteractive", action="store_true",
                    help="无交互：按默认勾选（不含数据目录）直接卸载 / no prompts, use default selection (excl. data)")
    args = ap.parse_args()

    comps = components()
    present = [c for c in comps if c["present"]()]
    if not present:
        log_step("未检测到已安装的 interest-memory 组件 / no installed components detected")
        return
    labels = [c["label"] for c in present]
    default = {i for i, c in enumerate(present) if c["id"] != "data"}

    log_step("选择要卸载的组件 / Select components to uninstall")
    if args.noninteractive:
        chosen = default
    else:
        try:
            chosen = pick_checklist(
                "选择要卸载的组件 / Select components to uninstall",
                labels,
                selected=default,
            )
        except GoBack:
            sys.exit(0)

    if not chosen:
        say("未选择任何组件，退出 / nothing selected, exiting")
        return

    picked = [present[i] for i in sorted(chosen)]
    log_step("将卸载以下组件 / Components to remove:")
    for c in picked:
        print(f"  - {c['label']}")

    if not args.noninteractive and not args.dry_run:
        try:
            if not ask_confirm("确认卸载以上组件？/ Confirm uninstall these components?",
                               default=False):
                say("已取消 / cancelled")
                return
        except GoBack:
            sys.exit(0)

    for c in picked:
        c["remove"](args.dry_run)

    if not args.dry_run:
        _hooks_hint()
        log_step("卸载完成 / uninstall complete")
    else:
        log_step("演练完成（未做任何修改）/ dry-run complete (nothing changed)")


def main() -> None:
    try:
        _run()
    except KeyboardInterrupt:
        print()
        warn("已取消（Ctrl+C）/ cancelled (Ctrl+C)")
        sys.exit(130)


if __name__ == "__main__":
    main()
