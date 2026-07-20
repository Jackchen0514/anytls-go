#!/usr/bin/env bash
# Upgrade an existing anytls-server install: downloads a prebuilt release
# binary from GitHub (latest by default) and restarts the systemd service.
# Credentials and the systemd unit created by install.sh are left untouched.
#
# Usage:
#   sudo bash update.sh [--version TAG]
#
# Requires network access to github.com to download the release archive.
# There is no fallback to a local build - if that's not an option for you,
# clone the repo and run `go build ./cmd/server` / `./cmd/client` yourself.
set -euo pipefail

REPO="Jackchen0514/anytls-go"
BIN_DIR="/usr/local/bin"
SERVICE_NAME="anytls-server"
VERSION="latest"

usage() { sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "请使用 root 权限运行 (例如: sudo bash update.sh)" >&2
  exit 1
fi

for tool in curl tar sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "需要 $tool，请先安装后重试" >&2; exit 1; }
done

case "$(uname -s)" in
  Linux) ;;
  *) echo "目前只提供 Linux 预编译包，当前系统: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "不支持的架构: $(uname -m)（目前只提供 amd64/arm64 预编译包）" >&2; exit 1 ;;
esac

if [[ "$VERSION" == "latest" ]]; then
  DOWNLOAD_BASE="https://github.com/$REPO/releases/latest/download"
else
  DOWNLOAD_BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
ASSET="anytls-linux-$ARCH.tar.gz"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> 下载预编译包: $DOWNLOAD_BASE/$ASSET"
if ! curl -fsSL "$DOWNLOAD_BASE/$ASSET" -o "$WORK_DIR/$ASSET"; then
  echo "下载失败: $DOWNLOAD_BASE/$ASSET" >&2
  echo "请检查网络是否能访问 GitHub，或用 --version 指定一个存在的发布版本号" >&2
  exit 1
fi
if ! curl -fsSL "$DOWNLOAD_BASE/SHA256SUMS" -o "$WORK_DIR/SHA256SUMS"; then
  echo "下载校验和文件失败: $DOWNLOAD_BASE/SHA256SUMS" >&2
  exit 1
fi

echo "==> 校验下载文件完整性"
EXPECTED="$(grep " $ASSET\$" "$WORK_DIR/SHA256SUMS" | awk '{print $1}')"
if [[ -z "$EXPECTED" ]]; then
  echo "校验和文件中找不到 $ASSET 对应记录" >&2
  exit 1
fi
ACTUAL="$(sha256sum "$WORK_DIR/$ASSET" | awk '{print $1}')"
if [[ "$EXPECTED" != "$ACTUAL" ]]; then
  echo "校验和不匹配，下载可能已损坏或被篡改，已中止更新" >&2
  echo "期望: $EXPECTED" >&2
  echo "实际: $ACTUAL" >&2
  exit 1
fi

echo "==> 解压并安装新二进制到 $BIN_DIR"
tar -xzf "$WORK_DIR/$ASSET" -C "$WORK_DIR"
install -m755 "$WORK_DIR/anytls-server" "$BIN_DIR/anytls-server"
install -m755 "$WORK_DIR/anytls-client" "$BIN_DIR/anytls-client"

if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$SERVICE_NAME.service" --no-legend 2>/dev/null | grep -q .; then
  echo "==> 重启服务"
  systemctl restart "$SERVICE_NAME"
  sleep 1
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    echo "服务已重启并正常运行。"
  else
    echo "服务重启后未处于运行状态，请检查: journalctl -u $SERVICE_NAME -n 50" >&2
    exit 1
  fi
else
  echo "未检测到 $SERVICE_NAME systemd 服务，二进制已更新，请手动重启对应进程。"
fi
