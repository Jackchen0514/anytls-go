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
#       --domain DOMAIN       issue a real Let's Encrypt certificate for this
#                             domain via DNS-01 (requires --cloudflare-token);
#                             without this, the server uses a generated
#                             self-signed certificate (clients need -insecure)
#       --cloudflare-token T  Cloudflare API token with Zone:DNS:Edit on the
#                             zone for --domain (required together with it)
#   -h, --help               show this help
#
# Re-running this script (e.g. with a newer --version) redeploys the binaries
# without rotating an already-generated password/API key, unless you pass
# -p/--password or --api-key explicitly. Re-running with the same --domain is
# also safe: acme.sh only re-issues when the existing cert is close to expiry.
#
# Requires network access to github.com to download the release archive
# (and, with --domain, to also fetch acme.sh, and to reach the Cloudflare
# API and Let's Encrypt to issue a certificate). There is no fallback to a
# local build - if downloading the release isn't an option for you, clone
# the repo and run `go build ./cmd/server` / `./cmd/client` yourself.
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
DOMAIN=""
CF_TOKEN=""

usage() { sed -n '2,34p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    -l|--listen) LISTEN="$2"; shift 2 ;;
    -p|--password) PASSWORD="$2"; shift 2 ;;
    --db) DB_PATH="$2"; shift 2 ;;
    --api-listen) API_LISTEN="$2"; shift 2 ;;
    --api-key) API_KEY="$2"; shift 2 ;;
    --no-api) ENABLE_API=0; shift ;;
    --version) VERSION="$2"; shift 2 ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --cloudflare-token) CF_TOKEN="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "请使用 root 权限运行 (例如: sudo bash install.sh)" >&2
  exit 1
fi

if [[ -n "$DOMAIN" && -z "$CF_TOKEN" ]]; then
  echo "使用 --domain 签发证书时必须同时提供 --cloudflare-token" >&2
  exit 1
fi
if [[ -z "$DOMAIN" && -n "$CF_TOKEN" ]]; then
  echo "提供了 --cloudflare-token 但未指定 --domain" >&2
  exit 1
fi

REQUIRED_TOOLS=(curl tar sha256sum)
[[ -n "$DOMAIN" ]] && REQUIRED_TOOLS+=(openssl)
for tool in "${REQUIRED_TOOLS[@]}"; do
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

CERT_ARGS=""
ACME_HOME="$CONF_DIR/acme.sh"
TLS_DIR="$CONF_DIR/tls"
if [[ -n "$DOMAIN" ]]; then
  mkdir -p "$TLS_DIR"
  # Fix ownership/mode on the directory itself right away, independent of
  # the `umask 077` set above for the credentials file (still in effect
  # here) and of whether the cert files exist yet: otherwise the directory
  # stays 700 root:root and the anytls user can't traverse into it to read
  # the certs even after their own ownership/mode is fixed later.
  chown "$SERVICE_USER":"$SERVICE_USER" "$TLS_DIR"
  chmod 750 "$TLS_DIR"
  # Paths only - the files themselves don't exist until the --issue/
  # --install-cert step further down, which needs the systemd unit (below)
  # to already exist so its reloadcmd has something to restart.
  CERT_ARGS=" -cert $TLS_DIR/fullchain.pem -key $TLS_DIR/privkey.pem"

  # Always (re)install: this is fast and idempotent (acme.sh preserves
  # existing account/domain config), and self-heals hosts that ended up with
  # an incomplete install from an older/interrupted run of this script -
  # e.g. only the bare acme.sh script with no dnsapi/ hooks copied alongside
  # it, which would otherwise keep failing forever since the script itself
  # already exists.
  echo "==> 安装/更新 acme.sh (用于向 Let's Encrypt 申请证书)"
  ACME_INSTALLER_DIR="$(mktemp -d)"
  # Fetch the full acme.sh repo archive (not just the acme.sh script) and
  # run --install from inside it, rather than piping through the
  # get.acme.sh convenience wrapper. That wrapper treats its first
  # positional argument as an `email=...` shorthand, which mangles a
  # leading `--home` flag into a broken `----home`. A lone acme.sh script
  # is also not enough on its own: DNS API hooks like dns_cf live in the
  # repo's dnsapi/ directory, which acme.sh looks for next to itself.
  if ! curl -fsSL https://github.com/acmesh-official/acme.sh/archive/refs/heads/master.tar.gz -o "$ACME_INSTALLER_DIR/acme.sh.tar.gz"; then
    echo "下载 acme.sh 失败，请检查网络" >&2
    exit 1
  fi
  tar -xzf "$ACME_INSTALLER_DIR/acme.sh.tar.gz" -C "$ACME_INSTALLER_DIR"
  ( cd "$ACME_INSTALLER_DIR/acme.sh-master" && sh ./acme.sh --install --home "$ACME_HOME" --config-home "$ACME_HOME/config" --no-profile )
  rm -rf "$ACME_INSTALLER_DIR"
fi

EXEC_START="$BIN_DIR/anytls-server -l $LISTEN -p $PASSWORD -db $DB_PATH$CERT_ARGS"
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
systemctl enable "$SERVICE_NAME"

if [[ -n "$DOMAIN" ]]; then
  echo "==> 通过 Let's Encrypt DNS-01 (Cloudflare) 为 $DOMAIN 申请证书"
  # Re-running --issue is a no-op unless the existing cert is close to expiry
  # (acme.sh's own logic): it then exits with code 2 (RENEW_SKIP), not 0 -
  # that's not a failure, it means a valid cert from an earlier run already
  # exists, so fall through to --install-cert below either way.
  if CF_Token="$CF_TOKEN" "$ACME_HOME/acme.sh" --home "$ACME_HOME" --config-home "$ACME_HOME/config" \
      --issue --dns dns_cf -d "$DOMAIN" --server letsencrypt --keylength 2048; then
    :
  else
    ISSUE_RC=$?
    if [[ "$ISSUE_RC" -ne 2 ]]; then
      echo "证书申请失败，请检查域名是否已通过 Cloudflare 解析、Token 是否有 Zone:DNS:Edit 权限" >&2
      exit 1
    fi
    echo "已有未到期的证书，跳过重新签发，直接安装现有证书"
  fi

  echo "==> 安装证书到 $TLS_DIR"
  # acme.sh writes fullchain/key as root:root (mode 600 on the key), which
  # the unprivileged anytls service user can't read - and it does this again
  # on every future automatic renewal, not just now. So the reloadcmd fixes
  # ownership/permissions *and* restarts, every time it runs; we also redo
  # it once more right after for this first run, in case the reloadcmd's
  # execution context ever behaves differently. Don't let a non-zero exit
  # here (e.g. the reloadcmd hiccuping) abort the whole script under set -e:
  # the fallback chown/chmod and restart+health-check below are exactly the
  # safety net for that, and can only run if we get to them.
  "$ACME_HOME/acme.sh" --home "$ACME_HOME" --config-home "$ACME_HOME/config" \
    --install-cert -d "$DOMAIN" \
    --key-file "$TLS_DIR/privkey.pem" \
    --fullchain-file "$TLS_DIR/fullchain.pem" \
    --reloadcmd "chown $SERVICE_USER:$SERVICE_USER $TLS_DIR/fullchain.pem $TLS_DIR/privkey.pem && chmod 640 $TLS_DIR/fullchain.pem $TLS_DIR/privkey.pem && systemctl restart $SERVICE_NAME" \
    || echo "警告: acme.sh --install-cert 报告了非零退出码，继续尝试自行修复权限并重启服务" >&2

  chown -R "$SERVICE_USER":"$SERVICE_USER" "$TLS_DIR"
  chmod 750 "$TLS_DIR"
  chmod 640 "$TLS_DIR"/*.pem
fi

systemctl restart "$SERVICE_NAME"

sleep 1
if ! systemctl is-active --quiet "$SERVICE_NAME"; then
  echo "服务未能正常启动，请查看: journalctl -u $SERVICE_NAME -n 50" >&2
  exit 1
fi

PORT="${LISTEN##*:}"
if [[ -n "$DOMAIN" ]]; then
  HOST="$DOMAIN"
  CONN_LINK="anytls://$PASSWORD@$HOST:$PORT?sni=$DOMAIN"
else
  HOST="$(curl -fsS4 --max-time 2 https://ifconfig.me 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}')"
  [[ -n "$HOST" ]] || HOST="<服务器IP>"
  CONN_LINK="anytls://$PASSWORD@$HOST:$PORT"
fi

echo
echo "======================================================"
echo " anytls-server 已安装并启动 (systemd service: $SERVICE_NAME)"
echo "------------------------------------------------------"
echo " 监听地址:   $LISTEN"
echo " 密码:       $PASSWORD"
echo " 连接链接:   $CONN_LINK"
if [[ -n "$DOMAIN" ]]; then
  echo " TLS 证书:   Let's Encrypt（$DOMAIN，acme.sh 已设置定时任务自动续期并重启服务）"
  echo "            示例客户端使用默认设置即可（不要加 -insecure），会正常校验证书"
else
  echo " TLS 证书:   自签名（每次重启进程都会重新生成，不做证书校验）"
  echo "            示例客户端必须加 -insecure，例如："
  echo "              anytls-client -s $HOST:$PORT -p $PASSWORD -insecure"
fi
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
