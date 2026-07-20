#!/usr/bin/env bash
# Upgrade an existing anytls-server install: pulls the latest source via git,
# rebuilds the binaries, and restarts the systemd service. Credentials and
# the systemd unit created by install.sh are left untouched.
#
# Usage:
#   sudo bash update.sh [--no-pull]
#
# Options:
#   --no-pull   skip `git pull`, just rebuild and redeploy the current
#               checkout as-is (useful if you already updated the source
#               yourself, or aren't tracking a git branch)
set -euo pipefail

BIN_DIR="/usr/local/bin"
SERVICE_NAME="anytls-server"

NO_PULL=0
usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-pull) NO_PULL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "请使用 root 权限运行 (例如: sudo bash update.sh)" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "未找到 go 工具链，请先安装 Go >= 1.24 (https://go.dev/dl/) 后重试" >&2
  exit 1
fi

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
cd "$SCRIPT_DIR"

if [[ ! -f go.mod ]] || ! grep -q '^module anytls$' go.mod; then
  echo "请在 anytls-go 源码目录下运行本脚本" >&2
  exit 1
fi

if [[ "$NO_PULL" -eq 0 ]]; then
  if [[ -d .git ]]; then
    if ! git diff --quiet || ! git diff --cached --quiet; then
      echo "检测到本地有未提交的修改，为避免覆盖已跳过 git pull。" >&2
      echo "请自行处理这些修改后重试，或加 --no-pull 直接用当前代码重新编译。" >&2
      exit 1
    fi
    echo "==> 拉取最新代码"
    CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
    git pull --ff-only origin "$CURRENT_BRANCH"
  else
    echo "警告: 当前目录不是 git 仓库，跳过拉取，直接使用现有源码重新编译。"
  fi
fi

echo "==> 编译 anytls-server / anytls-client"
BUILD_TMP="$(mktemp -d)"
trap 'rm -rf "$BUILD_TMP"' EXIT
go build -trimpath -o "$BUILD_TMP/anytls-server" ./cmd/server
go build -trimpath -o "$BUILD_TMP/anytls-client" ./cmd/client

echo "==> 安装新二进制到 $BIN_DIR"
install -m755 "$BUILD_TMP/anytls-server" "$BIN_DIR/anytls-server"
install -m755 "$BUILD_TMP/anytls-client" "$BIN_DIR/anytls-client"

if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$SERVICE_NAME.service" --no-legend 2>/dev/null | grep -q .; then
  echo "==> 重启服务"
  systemctl restart "$SERVICE_NAME"
  sleep 1
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    echo "服务已重启并正常运行。可用以下命令确认版本: journalctl -u $SERVICE_NAME -n 20"
  else
    echo "服务重启后未处于运行状态，请检查: journalctl -u $SERVICE_NAME -n 50" >&2
    exit 1
  fi
else
  echo "未检测到 $SERVICE_NAME systemd 服务，二进制已更新，请手动重启对应进程。"
fi
