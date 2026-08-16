#!/usr/bin/env bash
# ============================================================================
# interest-memory 安装器（薄壳）
#
# 仅负责三件事：
#   1. 检测依赖（python3 / go / curl 必需，node / npm 可选）
#   2. 定位本地源码，或从 GitHub 下载源码到临时目录
#   3. exec python3 scripts/install.py 唤起安装向导（全部安装逻辑在向导中）
#
# 升级：git pull 后重新执行本脚本即可。
# 卸载：删除用户级安装产物即可（源码目录下的 bin/、config.yaml 等）。
#
# ── 用法 ──────────────────────────────────────────────────────────────────
#   bash scripts/install.sh [向导参数...]
#     向导参数原样透传给 install.py（见 install.py --help），例如：
#       --noninteractive   无交互（CI 友好）：依赖缺失直接报错退出
#       --no-deps          跳过依赖检查
#       --dry-run          演练：只打印步骤，不执行
#       -h, --help         显示帮助（本薄壳用法）
#
# 退出码：0 成功；1 失败；2 参数错误；130 Ctrl+C。
# ============================================================================
set -euo pipefail

SOURCE_REPO="https://github.com/djasdh/interest-memory"

# ---- 运行期状态 -------------------------------------------------------------
SOURCE_DIR=""   # 源码目录（仓库或远程拉取的临时目录）
TMP_DIR=""      # 远程拉取的临时目录（退出时清理）

# ---- 输出辅助 ---------------------------------------------------------------
die()   { echo -e "\033[31m[错误]\033[0m $*" >&2; exit 1; }
say()   { echo -e "\033[32m[安装]\033[0m $*"; }
warn()  { echo -e "\033[33m[警告]\033[0m $*" >&2; }
step()  { echo -e "\033[36m==>\033[0m \033[1m$*\033[0m"; }
# 参数错误专用：按约定返回 2
arg_err() { echo -e "\033[31m[错误]\033[0m $*" >&2; echo "  用 --help 查看用法" >&2; exit 2; }

usage() {
  sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

# ---- 命令行状态（薄壳只关心非交互/无依赖/演练/帮助，其余透传给向导）-------
NONINTERACTIVE=0
NO_DEPS=0
DRY_RUN=0

for arg in "$@"; do
  case "$arg" in
    -h|--help) usage ;;
    --noninteractive) NONINTERACTIVE=1 ;;
    --no-deps) NO_DEPS=1 ;;
    --dry-run) DRY_RUN=1 ;;
  esac
done

# ---- OS 检测 ----------------------------------------------------------------
detect_os() {
  case "$(uname -s 2>/dev/null || echo unknown)" in
    Darwin) echo "macos" ;;
    *) if [[ -f /etc/os-release ]]; then
         local id; id="$(. /etc/os-release && echo "$ID")"
         case "$id" in
           arch|manjaro) echo "arch" ;;
           debian|ubuntu) echo "debian" ;;
           fedora|rhel|centos) echo "fedora" ;;
           alpine) echo "alpine" ;;
           *) echo "linux" ;;
         esac
       else echo "linux"; fi ;;
  esac
}

pkg_cmd() {
  case "$1" in
    arch)   echo "pacman -S --needed python go nodejs npm curl" ;;
    debian) echo "apt-get install -y python3 golang-go nodejs npm curl" ;;
    fedora) echo "dnf install -y python3 golang nodejs npm curl" ;;
    alpine) echo "apk add python3 go nodejs npm curl" ;;
    macos)  echo "brew install python go node npm curl" ;;
    *) echo "" ;;
  esac
}

manual_hint() {
  echo "请手动安装以下依赖后重试："
  echo "  - python3: https://www.python.org/downloads/"
  echo "  - go:      https://go.dev/dl/"
  echo "  - curl:    https://curl.se/download.html"
}

# ---- 依赖探测与安装 ----------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

MISSING=""
OPTIONAL_MISSING=""

probe_deps() {
  MISSING=""
  OPTIONAL_MISSING=""
  have python3 || MISSING+="python3 "
  have go     || MISSING+="go "
  have curl   || MISSING+="curl "
  have node   || OPTIONAL_MISSING+="node "
  have npm    || OPTIONAL_MISSING+="npm "
}

ensure_deps() {
  probe_deps
  [[ -z "$MISSING" ]] && [[ -z "$OPTIONAL_MISSING" ]] && { say "依赖齐全（python3 / go / curl / node / npm）"; return 0; }
  [[ -z "$MISSING" ]] || warn "缺少必需依赖: $MISSING"
  [[ -z "$OPTIONAL_MISSING" ]] || warn "缺少可选依赖: ${OPTIONAL_MISSING}(安装对应 agent bridge 时需要)"

  # 演练模式：不安装依赖，仅提示
  if [[ "$DRY_RUN" -eq 1 ]]; then
    return 0
  fi

  if [[ -z "$MISSING" ]]; then
    say "必需依赖齐全，可选依赖缺失不影响向导运行。"
    return 0
  fi

  if [[ "$NO_DEPS" -eq 1 ]]; then
    manual_hint; die "依赖缺失，安装终止（--no-deps 已指定）"
  fi
  if [[ "$NONINTERACTIVE" -eq 1 ]]; then
    manual_hint; die "非交互模式不自动安装依赖，请先手动安装后重试"
  fi

  local os cmd
  os="$(detect_os)"
  cmd="$(pkg_cmd "$os")"
  if [[ -z "$cmd" ]]; then
    manual_hint; die "无法为当前系统（$os）自动安装依赖"
  fi
  echo ""
  echo "将安装缺失依赖：$(echo "$MISSING" | tr -s ' ')"
  echo "执行命令（需要 root）：$cmd"
  read -r -p "是否执行？（输入 y 继续）[y/N]: " ans
  case "${ans,,}" in
    y|yes)
      if command -v sudo >/dev/null 2>&1 && [[ "$(id -u)" -ne 0 ]]; then
        sudo -v || die "sudo 认证失败，请手动安装依赖"
        sudo env PATH="$PATH" $cmd || die "依赖安装失败，请手动安装后重试"
      else
        $cmd || die "依赖安装失败，请手动安装后重试"
      fi
      probe_deps
      [[ -z "$MISSING" ]] || { manual_hint; die "仍有依赖未装上: $MISSING"; }
      say "依赖就绪"
      ;;
    *)
      manual_hint; die "已取消，请手动安装依赖后重试" ;;
  esac
}

# ---- 仓库定位（支持 curl | bash 远程执行）-----------------------------------
locate_source() {
  local script_dir repo
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]:-.}")" 2>/dev/null && pwd || true)"
  repo="$(cd "$script_dir/.." 2>/dev/null && pwd || true)"
  if [[ -f "$repo/scripts/install.py" && -f "$repo/scripts/install_curses.py" && -f "$repo/config.example.yaml" && -d "$repo/cmd/server" ]]; then
    SOURCE_DIR="$repo"
    say "源码目录: $SOURCE_DIR"
  else
    warn "未找到本地仓库源码，尝试远程拉取…"
    command -v curl >/dev/null 2>&1 || die "缺少 curl，无法远程拉取源码（请先安装 curl）"
    TMP_DIR="$(mktemp -d /tmp/im-install-XXXXXX)"
    say "远程拉取源码到 $TMP_DIR …"
    curl -fsSL "$SOURCE_REPO/archive/refs/heads/main.tar.gz" \
      | tar xz -C "$TMP_DIR" \
      || die "远程源码拉取失败（网络或仓库地址错误）"
    SOURCE_DIR="$TMP_DIR/interest-memory-main"
    [[ -f "$SOURCE_DIR/scripts/install.py" ]] || die "远程源码缺少 scripts/install.py，拉取可能不完整"
  fi
}

# 退出清理：总是删除远程拉取的临时目录
cleanup() {
  local rc=$?
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi
  exit $rc
}
trap cleanup EXIT

# ---- 主流程 -----------------------------------------------------------------
main() {
  ensure_deps
  locate_source
  local wizard="$SOURCE_DIR/scripts/install.py"
  [[ -f "$wizard" ]] || die "未找到安装向导: $wizard"

  if [[ "$DRY_RUN" -eq 1 ]]; then
    say "演练模式：将执行 python3 $wizard $*"
  fi

  step "唤起安装向导 / Launching installer wizard"
  exec python3 "$wizard" "$@"
}

main "$@"
