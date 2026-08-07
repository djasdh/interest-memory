#!/usr/bin/env bash
# interest-memory 安装器入口（薄封装）
#
# 前置依赖检查与自动安装（按发行版选择包管理器），然后转交
# scripts/install.py（Hermes 风格 curses TUI）。
#
# 用法：bash scripts/install.sh [install.py 参数...]
#   install.py 参数：--dry-run / --noninteractive / --server-only / --systemd / --help
set -euo pipefail

die() { echo -e "\033[31m[error]\033[0m $*" >&2; exit 1; }
say() { echo -e "\033[32m[install]\033[0m $*"; }
warn() { echo -e "\033[33m[warn]\033[0m $*"; }

# ---- 仓库定位（支持 curl | bash 远程执行）-----------------------------------
# 本地仓库内运行：BASH_SOURCE 指向 scripts/install.sh，直接定位。
# 远程管道运行：BASH_SOURCE 不可用，自动拉取发布源码到临时目录再转交。
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-.}")" 2>/dev/null && pwd || true)"
REPO="$(cd "$SCRIPT_DIR/.." 2>/dev/null && pwd || true)"
PY="$REPO/scripts/install.py"

if [[ ! -f "$PY" ]]; then
  REMOTE_TMP="$(mktemp -d)"
  say "远程安装模式：拉取源码到 $REMOTE_TMP ..."
  curl -fsSL "https://github.com/djasdh/interest-memory/archive/refs/heads/main.tar.gz" | tar xz -C "$REMOTE_TMP"
  REPO="$REMOTE_TMP/interest-memory-main"
  PY="$REPO/scripts/install.py"
  cd "$REPO"
fi

# curl | bash 模式下 stdin 是脚本管道；有真实终端则重绑，保证交互式 TUI 可用。
# 无终端环境（CI 等）保持管道 stdin，install.py 自行降级。
if [[ ! -t 0 ]] && [[ -e /dev/tty ]]; then
  exec < /dev/tty 2>/dev/null || true
fi

# 需要安装的依赖（--no-deps 时跳过自动安装）
NO_DEPS=0
PY_ARGS=()
for arg in "$@"; do
  if [[ "$arg" == "--no-deps" ]]; then
    NO_DEPS=1
  else
    PY_ARGS+=("$arg")
  fi
done

# Ctrl+C：友好提示并以 130（SIGINT）退出；python TUI 阶段由 install.py 自行处理。
trap 'echo; warn "已取消 (Ctrl+C)"; exit 130' INT

# ---- OS 检测 --------------------------------------------------------------
detect_os() {
  case "$(uname -s 2>/dev/null || echo Windows)" in
    Darwin) echo "macos" ;;
    *MINGW*|*MSYS*|*CYGWIN*|Windows_NT) echo "windows" ;;
    *) 
      if [ -f /etc/os-release ]; then
        local id; id="$(. /etc/os-release && echo "$ID")"
        case "$id" in
          arch|manjaro) echo "arch" ;;
          debian|ubuntu) echo "debian" ;;
          fedora|rhel|centos) echo "fedora" ;;
          alpine) echo "alpine" ;;
          *) echo "linux" ;;
        esac
      else
        echo "linux"
      fi
      ;;
  esac
}

# ---- 依赖探测 --------------------------------------------------------------
# 缺失的依赖以空格分隔写入全局 MISSING（"name" 形式，供安装与提示）。
# 每个工具映射到 <pkg_name>|<pkg_name_other> 的包名列表。
MISSING=""
have() { command -v "$1" >/dev/null 2>&1; }

probe_deps() {
  MISSING=""
  local os="$1"
  if [[ "$os" == "windows" ]]; then
    have python  || MISSING+="python "
    have python3 || MISSING+="python3 "
    have go      || MISSING+="go "
    have node    || MISSING+="node "
    have npm     || MISSING+="npm "
  else
    have python3 || MISSING+="python3 "
    have go      || MISSING+="go "
    have node    || MISSING+="node "
    have npm     || MISSING+="npm "
    have curl    || MISSING+="curl "
  fi
  # curses 属于 python 标准库；Windows python 通常没有，此处不强制（install.py 会自动降级）。
}

# ---- 按发行版生成安装命令（打印将安装的包名）----------------------------------
pkg_cmd() {
  local os="$1"
  case "$os" in
    arch)   echo "sudo pacman -S --needed python go nodejs npm curl" ;;
    debian) echo "sudo apt-get install -y python3 golang-go nodejs npm curl" ;;
    fedora) echo "sudo dnf install -y python3 golang nodejs npm curl" ;;
    alpine) echo "su -c 'apk add python3 go nodejs npm curl'" ;;
    macos)  echo "brew install python go node npm curl" ;;
    windows) echo "winget install Python.Python.3.12 GoLang.Go OpenJS.NodeJS.LTS" ;;
    *) echo "" ;;
  esac
}

# 打印缺失依赖对应的“用户手工安装”指引（每行一条）
manual_hint() {
  local os="$1"
  echo "请手动安装以下依赖后重试："
  for d in $MISSING; do
    case "$d" in
      python3|python) echo "  - python3:  https://www.python.org/downloads/" ;;
      go) echo "  - go:       https://go.dev/dl/" ;;
      node|npm) echo "  - node/npm: https://nodejs.org/" ;;
      curl) echo "  - curl:     https://curl.se/download.html" ;;
    esac
  done
}

# ---- 主流程 ----------------------------------------------------------------
main() {
  local os
  os="$(detect_os)"
  probe_deps "$os"

  if [[ -n "$MISSING" ]]; then
    warn "缺少依赖: $MISSING"
    if [[ "$NO_DEPS" -eq 1 ]]; then
      warn "--no-deps 已指定，跳过自动安装"
      manual_hint "$os"
      exit 1
    fi

    local cmd
    cmd="$(pkg_cmd "$os")"
    if [[ -z "$cmd" ]]; then
      warn "无法为当前系统（$os）自动安装依赖"
      manual_hint "$os"
      exit 1
    fi

    echo ""
    echo "将安装缺失依赖：$(echo $MISSING | tr ' ' ' ')"
    echo "执行命令: $cmd"
    echo ""
    if [[ ! -t 0 ]]; then
      warn "非交互终端，跳过依赖自动安装"
      manual_hint "$os"
      exit 1
    fi
    read -r -p "是否执行？（需要 sudo 密码，输入 y 继续）[y/N]: " ans
    case "${ans,,}" in
      y|yes)
        # 先验证/缓存 sudo 凭证（会提示输入密码），再执行安装。
        if ! sudo -v; then
          warn "sudo 认证失败，请手动安装依赖"
          manual_hint "$os"
          exit 1
        fi
        if ! eval "$cmd"; then
          warn "依赖安装失败，请手动安装后重试"
          manual_hint "$os"
          exit 1
        fi
        # 重新探测确认
        probe_deps "$os"
        if [[ -n "$MISSING" ]]; then
          warn "仍有依赖未装上: $MISSING"
          manual_hint "$os"
          exit 1
        fi
        say "依赖就绪"
        ;;
      *)
        warn "取消安装。请手动安装依赖后重试。"
        manual_hint "$os"
        exit 1
        ;;
    esac
  else
    say "依赖齐全"
  fi

  trap - INT  # 恢复默认 SIGINT，转交 install.py 的键盘处理
  exec python3 "$PY" "${PY_ARGS[@]}"
}

main "$@"
