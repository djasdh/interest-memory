#!/usr/bin/env bash
# ============================================================================
# interest-memory 卸载器（薄壳）
#
# 仅负责三件事：
#   1. 检测 python3（必需；curl 仅远程拉取源码时需要）
#   2. 定位本地源码（含 scripts/uninstall.py），必要时从 GitHub 下载
#   3. exec python3 scripts/uninstall.py 唤起卸载向导（TUI 勾选要卸载的组件）
#
# ── 用法 ──────────────────────────────────────────────────────────────────
#   bash scripts/uninstall.sh [向导参数...]
#     向导参数原样透传给 uninstall.py，例如：
#       --noninteractive   无交互：按默认勾选（不含数据目录）直接卸载
#       --dry-run          演练：只打印将卸载的内容，不执行
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
say()   { echo -e "\033[32m[卸载]\033[0m $*"; }
warn()  { echo -e "\033[33m[警告]\033[0m $*" >&2; }
step()  { echo -e "\033[36m==>\033[0m \033[1m$*\033[0m"; }
# 参数错误专用：按约定返回 2
arg_err() { echo -e "\033[31m[错误]\033[0m $*" >&2; echo "  用 --help 查看用法" >&2; exit 2; }

usage() {
  sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

# ---- 命令行状态（其余参数透传给卸载向导）-----------------------------------
NONINTERACTIVE=0
DRY_RUN=0

for arg in "$@"; do
  case "$arg" in
    -h|--help) usage ;;
    --noninteractive) NONINTERACTIVE=1 ;;
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
    arch)   echo "pacman -S --needed python3" ;;
    debian) echo "apt-get install -y python3" ;;
    fedora) echo "dnf install -y python3" ;;
    alpine) echo "apk add python3" ;;
    macos)  echo "brew install python" ;;
    *) echo "" ;;
  esac
}

# ---- python3 检查 ------------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

ensure_python() {
  have python3 && { say "python3 OK"; return 0; }
  warn "缺少 python3，无法运行卸载向导"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    return 0
  fi
  if [[ "$NONINTERACTIVE" -eq 1 ]]; then
    die "缺少 python3，请先安装后重试"
  fi
  local os cmd
  os="$(detect_os)"
  cmd="$(pkg_cmd "$os")"
  if [[ -z "$cmd" ]]; then
    die "请手动安装 python3（https://www.python.org/downloads/）后重试"
  fi
  echo "将安装 python3：$cmd"
  read -r -p "是否执行？（输入 y 继续）[y/N]: " ans
  case "${ans,,}" in
    y|yes)
      if command -v sudo >/dev/null 2>&1 && [[ "$(id -u)" -ne 0 ]]; then
        sudo -v || die "sudo 认证失败，请手动安装 python3"
        sudo env PATH="$PATH" $cmd || die "python3 安装失败"
      else
        $cmd || die "python3 安装失败"
      fi
      have python3 || die "python3 仍未就绪"
      say "python3 就绪"
      ;;
    *)
      die "已取消，请先安装 python3 后重试" ;;
  esac
}

# ---- 仓库定位（支持 curl | bash 远程执行）-----------------------------------
locate_source() {
  local script_dir repo
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]:-.}")" 2>/dev/null && pwd || true)"
  repo="$(cd "$script_dir/.." 2>/dev/null && pwd || true)"
  if [[ -f "$repo/scripts/uninstall.py" && -f "$repo/scripts/install_curses.py" && -f "$repo/config.example.yaml" ]]; then
    SOURCE_DIR="$repo"
    say "源码目录: $SOURCE_DIR"
  else
    warn "未找到本地仓库源码，尝试远程拉取…"
    command -v curl >/dev/null 2>&1 || die "缺少 curl，无法远程拉取源码（请先安装 curl）"
    TMP_DIR="$(mktemp -d /tmp/im-uninstall-XXXXXX)"
    say "远程拉取源码到 $TMP_DIR …"
    curl -fsSL "$SOURCE_REPO/archive/refs/heads/main.tar.gz" \
      | tar xz -C "$TMP_DIR" \
      || die "远程源码拉取失败（网络或仓库地址错误）"
    SOURCE_DIR="$TMP_DIR/interest-memory-main"
    [[ -f "$SOURCE_DIR/scripts/uninstall.py" ]] || die "远程源码缺少 scripts/uninstall.py，拉取可能不完整"
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
  ensure_python
  locate_source
  local wizard="$SOURCE_DIR/scripts/uninstall.py"
  [[ -f "$wizard" ]] || die "未找到卸载向导: $wizard"

  if [[ "$DRY_RUN" -eq 1 ]]; then
    say "演练模式：将执行 python3 $wizard $*"
  fi

  step "唤起卸载向导 / Launching uninstall wizard"
  exec python3 "$wizard" "$@"
}

main "$@"
