#!/usr/bin/env bash
# Uninstall anytls-server (installed via install.sh).
#
# By default the sqlite user database and generated credentials are kept, in
# case you're only reinstalling. Pass --purge to remove them too.
#
# Usage:
#   sudo bash uninstall.sh [--purge]
set -euo pipefail

BIN_DIR="/usr/local/bin"
DATA_DIR="/var/lib/anytls"
CONF_DIR="/etc/anytls"
UNIT_FILE="/etc/systemd/system/anytls-server.service"
SERVICE_NAME="anytls-server"
SERVICE_USER="anytls"

PURGE=0
usage() { sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --purge) PURGE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "请使用 root 权限运行 (例如: sudo bash uninstall.sh)" >&2
  exit 1
fi

if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$SERVICE_NAME.service" --no-legend 2>/dev/null | grep -q .; then
  echo "==> 停止并禁用 systemd 服务"
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  systemctl disable "$SERVICE_NAME" 2>/dev/null || true
fi
rm -f "$UNIT_FILE"
command -v systemctl >/dev/null 2>&1 && systemctl daemon-reload || true

echo "==> 移除二进制文件"
rm -f "$BIN_DIR/anytls-server" "$BIN_DIR/anytls-client"

if [[ "$PURGE" -eq 1 ]]; then
  echo "==> 清除用户数据库与凭据 ($DATA_DIR, $CONF_DIR)"
  rm -rf "$DATA_DIR" "$CONF_DIR"
  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    userdel "$SERVICE_USER" 2>/dev/null || true
  fi
  echo "已完全卸载 anytls-server，用户数据库与凭据已删除。"
else
  echo "已卸载 anytls-server。"
  echo "用户数据库 ($DATA_DIR) 与凭据 ($CONF_DIR) 已保留；如需彻底清除，运行: sudo bash uninstall.sh --purge"
fi
