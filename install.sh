#!/usr/bin/env bash
# Install anytls-server as a systemd service, using a prebuilt release
# binary downloaded from GitHub (no source checkout or Go toolchain needed).
#
# Usage:
#   sudo bash install.sh [options]
#   curl -fsSL https://raw.githubusercontent.com/Jackchen0514/anytls-go/main/install.sh | sudo bash
#
# Options:
#   -l, --listen ADDR        server listen address (default: 0.0.0.0:8443)
#   -p, --password PASS      connection password (default: random, generated once)
#       --db PATH             sqlite user database path (default: /var/lib/anytls/anytls.db)
#       --api-listen ADDR    admin API listen address (default: 127.0.0.1:8843)
#       --api-key KEY        admin API key (default: random, generated once)
#       --no-api             disable the admin API entirely
#       --version TAG         release tag to install (default: latest)
#   -h, --help               show this help
#
# Re-running this script (e.g. with a newer --version) redeploys the binaries
# without rotating an already-generated password/API key, unless you pass
# -p/--password or --api-key explicitly.
#
# Requires network access to github.com to download the release archive.
# There is no fallback to a local build - if that's not an option for you,
# clone the repo and run `go build ./cmd/server` / `./cmd/client` yourself.
set -euo pipefail

REPO="Jackchen0514/anytls-go"
BIN_DIR="/usr/local/bin"
DATA_DIR="/var/lib/anytls"
CONF_DIR="/etc/anytls"
CRED_FILE="$CONF_DIR/credentials.env"
UNIT_FILE="/etc/systemd/system/anytls-server.service"
SERVICE_NAME="anytls-server"
SERVICE_USER="anytls"

LISTEN="0.0.0.0:8443"
PASSWORD=""
DB_PATH="$DATA_DIR/anytls.db"
API_LISTEN="127.0.0.1:8843"
API_KEY=""
ENABLE_API=1
VERSION="latest"

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    -l|--listen) LISTEN="$2"; shift 2 ;;
    -p|--password) PASSWORD="$2"; shift 2 ;;
    --db) DB_PATH="$2"; shift 2 ;;
    --api-listen) API_LISTEN="$2"; shift 2 ;;
    --api-key) API_KEY="$2"; shift 2 ;;
    --no-api) ENABLE_API=0; shift ;;
    --version) VERSION="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "请使用 root 权限运行 (例如: sudo bash install.sh)" >&2
  exit 1
fi

for tool in curl tar sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "需要 $tool，请先安装后重试" >&2; exit 1; }
done

if ! command -v systemctl >/dev/null 2>&1; then
  echo "警告: 未检测到 systemd，将只安装二进制文件，不会创建系统服务。" >&2
fi

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
  echo "校验和不匹配，下载可能已损坏或被篡改，已中止安装" >&2
  echo "期望: $EXPECTED" >&2
  echo "实际: $ACTUAL" >&2
  exit 1
fi

echo "==> 解压并安装二进制到 $BIN_DIR"
tar -xzf "$WORK_DIR/$ASSET" -C "$WORK_DIR"
install -m755 "$WORK_DIR/anytls-server" "$BIN_DIR/anytls-server"
install -m755 "$WORK_DIR/anytls-client" "$BIN_DIR/anytls-client"

if ! command -v systemctl >/dev/null 2>&1; then
  echo "完成。手动运行示例:"
  echo "  anytls-server -l $LISTEN -p <密码> -db $DB_PATH"
  exit 0
fi

gen_secret() {
  # `head -c` closing the pipe early sends tr a SIGPIPE; under `set -o
  # pipefail` (in effect for the rest of this script) that would make the
  # whole pipeline - and thus `set -e` - abort. $(...) always runs in a
  # subshell, so scoping the disable to this function body only affects
  # that subshell, not the caller's pipefail setting.
  set +o pipefail
  LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32
}

# Re-running install.sh should not silently rotate credentials that are
# already in use by deployed clients/integrations.
if [[ -f "$CRED_FILE" ]]; then
  # shellcheck disable=SC1090
  source "$CRED_FILE"
  [[ -z "$PASSWORD" ]] && PASSWORD="${PREV_PASSWORD:-}"
  [[ -z "$API_KEY" ]] && API_KEY="${PREV_API_KEY:-}"
fi
[[ -n "$PASSWORD" ]] || PASSWORD="$(gen_secret)"
if [[ "$ENABLE_API" -eq 1 && -z "$API_KEY" ]]; then
  API_KEY="$(gen_secret)"
fi

if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  echo "==> 创建系统用户 $SERVICE_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

mkdir -p "$DATA_DIR" "$CONF_DIR"
chown -R "$SERVICE_USER":"$SERVICE_USER" "$DATA_DIR"

umask 077
{
  echo "# generated by install.sh, keep this file secret"
  echo "PREV_PASSWORD=$PASSWORD"
  echo "PREV_API_KEY=$API_KEY"
} > "$CRED_FILE"
chown root:root "$CRED_FILE"
chmod 600 "$CRED_FILE"

EXEC_START="$BIN_DIR/anytls-server -l $LISTEN -p $PASSWORD -db $DB_PATH"
if [[ "$ENABLE_API" -eq 1 ]]; then
  EXEC_START="$EXEC_START -api-listen $API_LISTEN -api-key $API_KEY"
fi

echo "==> 写入 systemd 单元 $UNIT_FILE"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=AnyTLS Server
After=network.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
ExecStart=$EXEC_START
Restart=on-failure
RestartSec=3
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=$DATA_DIR
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

sleep 1
if ! systemctl is-active --quiet "$SERVICE_NAME"; then
  echo "服务未能正常启动，请查看: journalctl -u $SERVICE_NAME -n 50" >&2
  exit 1
fi

HOST="$(curl -fsS4 --max-time 2 https://ifconfig.me 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}')"
[[ -n "$HOST" ]] || HOST="<服务器IP>"
PORT="${LISTEN##*:}"

echo
echo "======================================================"
echo " anytls-server 已安装并启动 (systemd service: $SERVICE_NAME)"
echo "------------------------------------------------------"
echo " 监听地址:   $LISTEN"
echo " 密码:       $PASSWORD"
echo " 连接链接:   anytls://$PASSWORD@$HOST:$PORT"
if [[ "$ENABLE_API" -eq 1 ]]; then
  echo " 管理 API:   http://$API_LISTEN/  (Key: $API_KEY)"
fi
echo " 用户数据库: $DB_PATH"
echo " 凭据留存于: $CRED_FILE (重新运行本脚本不会更换密码/Key)"
echo "------------------------------------------------------"
echo " 常用命令:"
echo "   systemctl status $SERVICE_NAME"
echo "   journalctl -u $SERVICE_NAME -f"
echo "======================================================"
