#!/bin/sh
# Clusterheart host agent installer.
#
#   curl -fsSL https://clusterheart-server.clusterheart.com/install-host.sh | sudo sh -s -- --id <server-id> --token <token>
#   curl -fsSL https://clusterheart-server.clusterheart.com/install-host.sh | sudo sh -s -- --update
#   curl -fsSL https://clusterheart-server.clusterheart.com/install-host.sh | sudo sh -s -- --uninstall
#
# Installs a small read-only agent that runs as its own unprivileged user,
# reports this server's health to Clusterheart over outbound HTTPS, and never
# changes anything on the machine. No ports are opened.
set -eu

CP_URL="https://clusterheart-server.clusterheart.com/cp"
BIN_BASE="https://0s9ykblnlp7vbe8p.public.blob.vercel-storage.com/sre"
ID=""
TOKEN=""
UNINSTALL=""
UPDATE=""
SERVICE=clusterheart-host-agent
BIN=/usr/local/bin/clusterheart-host-agent
ENV_FILE=/etc/clusterheart/host-agent.env
UNIT=/etc/systemd/system/$SERVICE.service

while [ $# -gt 0 ]; do
  case "$1" in
    --id) ID="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --url) CP_URL="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --update) UPDATE=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 1 ;;
  esac
done

if [ "$(id -u)" != 0 ]; then
  echo "Please run with sudo." >&2
  exit 1
fi
if ! command -v systemctl >/dev/null 2>&1; then
  echo "This installer needs a systemd-based Linux server." >&2
  exit 1
fi

if [ -n "$UNINSTALL" ]; then
  systemctl disable --now "$SERVICE" 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  rm -rf /etc/clusterheart
  systemctl daemon-reload
  id clusterheart >/dev/null 2>&1 && userdel clusterheart 2>/dev/null || true
  echo "Clusterheart host agent removed."
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Unsupported CPU architecture: $(uname -m)" >&2; exit 1 ;;
esac

download_agent() {
  echo "Downloading the Clusterheart host agent ($ARCH)..."
  TMP="$(mktemp)"
  trap 'rm -f "$TMP"' EXIT
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$BIN_BASE/clusterheart-host-agent-linux-$ARCH" -o "$TMP"
  else
    wget -qO "$TMP" "$BIN_BASE/clusterheart-host-agent-linux-$ARCH"
  fi
  install -m 0755 "$TMP" "$BIN"
}

check_running() {
  sleep 2
  if systemctl is-active --quiet "$SERVICE"; then
    echo "$1"
  else
    echo "The agent did not start. Check: journalctl -u $SERVICE -n 50" >&2
    exit 1
  fi
}

# --update: newest agent, same server id and token.
if [ -n "$UPDATE" ]; then
  if [ ! -f "$ENV_FILE" ]; then
    echo "The agent is not installed on this server. Use the install command from the dashboard." >&2
    exit 1
  fi
  download_agent
  systemctl restart "$SERVICE"
  check_running "Clusterheart host agent updated and running."
  exit 0
fi

if [ -z "$ID" ] || [ -z "$TOKEN" ]; then
  echo "Usage: install-host.sh --id <server-id> --token <token>  (copy the command from the dashboard)" >&2
  exit 1
fi

download_agent

# Unprivileged user; the journal groups let it read system logs.
if ! id clusterheart >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin clusterheart 2>/dev/null \
    || useradd -r -M -s /sbin/nologin clusterheart
fi
for g in systemd-journal adm; do
  if getent group "$g" >/dev/null 2>&1; then usermod -aG "$g" clusterheart; fi
done

mkdir -p /etc/clusterheart
umask 077
cat > "$ENV_FILE" <<EOF
CONTROLPLANE_URL=$CP_URL
CLUSTER_ID=$ID
AGENT_TOKEN=$TOKEN
EOF
chown root:clusterheart "$ENV_FILE"
chmod 0640 "$ENV_FILE"
umask 022

cat > "$UNIT" <<EOF
[Unit]
Description=Clusterheart host agent (read-only)
After=network-online.target
Wants=network-online.target

[Service]
User=clusterheart
Group=clusterheart
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
ProtectControlGroups=yes
ProtectKernelModules=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryMax=128M
CPUQuota=20%

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1
systemctl restart "$SERVICE"
check_running "Clusterheart host agent is running. This server will show as Connected within a minute."
