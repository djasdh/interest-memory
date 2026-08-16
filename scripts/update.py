#!/usr/bin/env python3
"""interest-memory 更新器 / updater — 非交互升级。

流程：
  1. 探测已安装组件（服务端 / 各 agent bridge / systemd 用户服务）
  2. git pull --ff-only 拉取最新源码（若本地是 git 仓库；失败仅警告不阻塞）
  3. go build 重建 bin/interest-memory-server
  4. 更新已安装的 bridge（重新复制，非交互）
  5. systemd 用户服务在运行时则重启

config.yaml / ~/.config/interest-memory.env / 数据目录一律保留不动。

用法：
    python3 scripts/update.py                # 默认：git pull + 构建 + 更新 bridge + 重启
    python3 scripts/update.py --dry-run      # 演练：打印将执行的操作，不执行
    python3 scripts/update.py --no-service   # 不重启 systemd 服务
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
SYSTEMD_UNIT = Path.home() / ".config" / "systemd" / "user" / "interest-memory.service"
OLD_REPO_PATH = "/home/umr/Projects/agent-adapt/interest-memory"
DSH_PACKAGE = os.environ.get("INTEREST_DSH_PACKAGE", "@djasdh/interest-memory-dsh-bridge")

sys.path.insert(0, str(Path(__file__).resolve().parent))
from install_curses import show_msg  # noqa: E402


def say(msg: str) -> None:
    print(f"\033[32m[更新]\033[0m {msg}")


def warn(msg: str) -> None:
    print(f"\033[33m[警告]\033[0m {msg}", file=sys.stderr)


def log_step(msg: str) -> None:
    print(f"\033[36m==>\033[0m \033[1m{msg}\033[0m")


def which(name: str) -> bool:
    return shutil.which(name) is not None


# ---------------------------------------------------------------------------
# bridge 目标路径（与 install.py / uninstall.py 一致）
# ---------------------------------------------------------------------------
OPENCODE_FILES = [
    Path.home() / ".config" / "opencode" / "plugin" / "memory.ts",
    Path.home() / ".config" / "opencode" / "plugin" / "memory-lib.ts",
]
PI_DIR = Path.home() / ".pi" / "agent" / "extensions" / "interest-memory"
OPENCLAW_DIR = Path.home() / ".openclaw" / "extensions" / "interest-memory"
HERMES_DIR = Path(os.environ.get("HERMES_HOME") or str(Path.home() / ".hermes")) / "plugins" / "interest"
CLAUDE_DIR = Path.home() / ".claude" / "plugins" / "interest-memory"
CODEX_CFG = Path.home() / ".codex" / "config.toml"
REASONIX_DIR = Path.home() / ".local" / "share" / "interest-memory" / "reasonix-plugin"

BRIDGE_IDS = ["opencode", "pi", "openclaw", "hermes", "claudecode", "codex", "reasonix", "dsh"]


def _has_opencode() -> bool:
    return any(p.exists() for p in OPENCODE_FILES)


def _has_pi() -> bool:
    return PI_DIR.exists() or PI_DIR.is_symlink()


def _has_openclaw() -> bool:
    return OPENCLAW_DIR.exists() or OPENCLAW_DIR.is_symlink()


def _has_hermes() -> bool:
    return HERMES_DIR.exists() or HERMES_DIR.is_symlink()


def _has_claudecode() -> bool:
    return CLAUDE_DIR.exists() or CLAUDE_DIR.is_symlink()


def _has_codex() -> bool:
    return CODEX_CFG.exists() and "mcp_servers.interest-memory" in CODEX_CFG.read_text(encoding="utf-8")


def _has_reasonix() -> bool:
    return REASONIX_DIR.exists() or REASONIX_DIR.is_symlink()


# ---------------------------------------------------------------------------
# 1. git pull
# ---------------------------------------------------------------------------
def git_pull(dry: bool) -> bool:
    if not (REPO / ".git").exists():
        warn("源码目录不是 git 仓库，跳过 git pull（请手动拉取最新代码后重试）")
        return False
    log_step("拉取最新代码 / git pull --ff-only")
    if dry:
        print(f"  git -C {REPO} pull --ff-only")
        return True
    r = subprocess.run(["git", "-C", str(REPO), "pull", "--ff-only"],
                       capture_output=True, text=True)
    if r.returncode != 0:
        warn(f"git pull 失败：{r.stderr.strip() or r.stdout.strip()}")
        warn("可能本地有未提交改动或非快进合并；继续用当前源码构建。")
        return False
    say(r.stdout.strip() or "已是最新 / up to date")
    return True


# ---------------------------------------------------------------------------
# 2. 构建服务端
# ---------------------------------------------------------------------------
def build_server(dry: bool) -> bool:
    log_step("构建服务端 / Building server")
    if not which("go"):
        warn("缺少 go，跳过构建（请安装 go 1.25+ 后重试）")
        return False
    if dry:
        print(f"  go build -o {BIN} ./cmd/server (cwd={REPO})")
        return True
    (REPO / "bin").mkdir(parents=True, exist_ok=True)
    r = subprocess.run(["go", "build", "-o", str(BIN), "./cmd/server"], cwd=str(REPO))
    if r.returncode != 0:
        warn("go build 失败（见上方编译错误）")
        return False
    say(f"二进制 / binary: {BIN}")
    return True


# ---------------------------------------------------------------------------
# 3. 更新已安装 bridge（非交互）
# ---------------------------------------------------------------------------
def _copy_file(src: Path, dst: Path, dry: bool) -> None:
    if dry:
        print(f"  cp {src} {dst}")
        return
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dst)


def _copy_tree(src: Path, dst: Path, dry: bool) -> None:
    if dry:
        print(f"  cp -r {src} {dst}")
        return
    if dst.exists() or dst.is_symlink():
        shutil.rmtree(dst, ignore_errors=True)
    shutil.copytree(src, dst)


def _update_opencode(dry: bool) -> None:
    log_step("更新 opencode bridge / Updating opencode bridge")
    src = REPO / "bridge" / "opencode"
    _copy_file(src / "memory.ts", OPENCODE_FILES[0], dry)
    _copy_file(src / "memory-lib.ts", OPENCODE_FILES[1], dry)


def _update_pi(dry: bool) -> None:
    log_step("更新 pi bridge / Updating pi bridge")
    src = REPO / "bridge" / "pi"
    if dry:
        print(f"  cp {src/'memory.ts'} {PI_DIR/'index.ts'}")
        print(f"  cp {src/'lib.ts'} {PI_DIR/'lib.ts'}")
        return
    PI_DIR.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src / "memory.ts", PI_DIR / "index.ts")
    shutil.copy2(src / "lib.ts", PI_DIR / "lib.ts")


def _update_openclaw(dry: bool) -> None:
    log_step("更新 openclaw bridge / Updating openclaw bridge")
    _copy_tree(REPO / "bridge" / "openclaw" / "interest-memory", OPENCLAW_DIR, dry)
    if dry:
        print(f"  (cd {OPENCLAW_DIR} && npm install --no-audit --no-fund)")
    else:
        subprocess.run("npm install --no-audit --no-fund", shell=True, cwd=str(OPENCLAW_DIR),
                       check=False)


def _update_hermes(dry: bool) -> None:
    log_step("更新 hermes bridge / Updating hermes bridge")
    src = REPO / "bridge" / "hermes"
    if dry:
        print(f"  cp -r {src}/. {HERMES_DIR}/")
        return
    HERMES_DIR.mkdir(parents=True, exist_ok=True)
    for item in src.iterdir():
        if item.name == "__pycache__":
            continue
        dst = HERMES_DIR / item.name
        if item.is_dir():
            if dst.exists() or dst.is_symlink():
                shutil.rmtree(dst, ignore_errors=True)
            shutil.copytree(item, dst)
        else:
            shutil.copy2(item, dst)


def _update_claudecode(dry: bool) -> None:
    log_step("更新 claudecode bridge / Updating claudecode bridge")
    _copy_tree(REPO / "bridge" / "claudecode", CLAUDE_DIR, dry)
    mcp = CLAUDE_DIR / ".mcp.json"
    if dry:
        print(f"  sed -i s#__INTEREST_REPO__#{REPO}#g {mcp}")
    else:
        mcp.write_text(mcp.read_text(encoding="utf-8").replace("__INTEREST_REPO__", str(REPO)),
                       encoding="utf-8")


def _update_codex(dry: bool) -> None:
    log_step("更新 codex bridge（config.toml 路径）/ Updating codex bridge")
    if not CODEX_CFG.exists():
        return
    if dry:
        print(f"  更新 {CODEX_CFG} 中 MCP 路径为 {REPO}")
        return
    t = CODEX_CFG.read_text(encoding="utf-8")
    t = t.replace(OLD_REPO_PATH, str(REPO))
    new = re.sub(r'args = \["([^"]*)/bridge/mcp-server/server\.ts"\]',
                 lambda m: f'args = ["{REPO}/bridge/mcp-server/server.ts"]', t)
    if new != t:
        CODEX_CFG.write_text(new, encoding="utf-8")
        say(f"已更新 / updated MCP 路径 in {CODEX_CFG}")
    else:
        say("路径已是最新 / paths already current")


def _update_reasonix(dry: bool) -> None:
    log_step("更新 reasonix bridge / Updating reasonix bridge")
    _copy_tree(REPO / "bridge" / "reasonix", REASONIX_DIR, dry)
    manifest = REASONIX_DIR / "reasonix-plugin.json"
    if dry:
        print(f"  sed -i s#__INTEREST_REPO__#{REPO}#g {manifest}")
        print("  reasonix plugin remove interest-memory --yes && reasonix plugin install ...")
        return
    manifest.write_text(manifest.read_text(encoding="utf-8").replace("__INTEREST_REPO__", str(REPO)),
                        encoding="utf-8")
    if which("reasonix"):
        subprocess.run("reasonix plugin remove interest-memory --yes", shell=True, capture_output=True)
        subprocess.run(f'reasonix plugin install "{REASONIX_DIR}" --link --replace --yes',
                       shell=True, check=False)


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


def _update_dsh(dry: bool) -> None:
    log_step("dsh bridge 需手动更新 / dsh bridge update is manual")
    for prof in _dsh_installed_profiles():
        print(f"  dsh plugin --profile {prof.name} add {DSH_PACKAGE}")


BRIDGE_UPDATERS = {
    "opencode": _update_opencode,
    "pi": _update_pi,
    "openclaw": _update_openclaw,
    "hermes": _update_hermes,
    "claudecode": _update_claudecode,
    "codex": _update_codex,
    "reasonix": _update_reasonix,
    "dsh": _update_dsh,
}


def _installed_bridges() -> list[str]:
    checks = {
        "opencode": _has_opencode,
        "pi": _has_pi,
        "openclaw": _has_openclaw,
        "hermes": _has_hermes,
        "claudecode": _has_claudecode,
        "codex": _has_codex,
        "reasonix": _has_reasonix,
        "dsh": _has_dsh,
    }
    return [bid for bid in BRIDGE_IDS if checks[bid]()]


# ---------------------------------------------------------------------------
# 4. 重启 systemd 用户服务
# ---------------------------------------------------------------------------
def restart_systemd(dry: bool, no_service: bool) -> None:
    if no_service:
        say("跳过 systemd 服务重启（--no-service）")
        return
    if not SYSTEMD_UNIT.exists():
        say("未注册 systemd 用户服务，跳过重启")
        return
    if not which("systemctl"):
        warn("未找到 systemctl，跳过重启")
        return
    log_step("重启 systemd 用户服务 / Restarting systemd user service")
    if dry:
        print("  systemctl --user is-active interest-memory.service")
        print("  systemctl --user restart interest-memory.service")
        return
    r = subprocess.run(["systemctl", "--user", "is-active", "interest-memory.service"],
                       capture_output=True, text=True)
    if r.stdout.strip() != "active":
        say("服务未运行，跳过重启")
        return
    subprocess.run(["systemctl", "--user", "restart", "interest-memory.service"],
                   check=False)
    say("服务已重启 / restarted interest-memory.service")


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------
def _run() -> None:
    ap = argparse.ArgumentParser(description="interest-memory 更新器 / updater")
    ap.add_argument("--dry-run", action="store_true",
                    help="演练：只打印将执行的操作，不执行 / print steps only, no changes")
    ap.add_argument("--no-service", action="store_true",
                    help="不重启 systemd 服务 / do not restart systemd service")
    args = ap.parse_args()

    has_server = BIN.exists() or CONFIG.exists()
    bridges = _installed_bridges()
    if not has_server and not bridges:
        log_step("未检测到已安装的 interest-memory 组件 / no installed components detected")
        say("请先运行 bash scripts/install.sh 完成安装")
        return

    say(f"将更新：服务端 {'✓' if has_server else '—'} / bridges: {', '.join(bridges) if bridges else '（无）'}")

    git_pull(args.dry_run)

    if has_server:
        build_server(args.dry_run)
    else:
        say("未检测到服务端，跳过构建")

    for bid in bridges:
        BRIDGE_UPDATERS[bid](args.dry_run)

    restart_systemd(args.dry_run, args.no_service)

    log_step("更新完成 / update complete")


def main() -> None:
    try:
        _run()
    except KeyboardInterrupt:
        print()
        warn("已取消（Ctrl+C）/ cancelled (Ctrl+C)")
        sys.exit(130)


if __name__ == "__main__":
    main()
