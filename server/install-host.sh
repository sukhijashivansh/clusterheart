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
DOCKER_UNIT=clusterheart-docker-ps
DOCKER_HELPER=/usr/local/libexec/$DOCKER_UNIT

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

remove_docker_timer() {
  systemctl disable --now "$DOCKER_UNIT.timer" 2>/dev/null || true
  rm -f "/etc/systemd/system/$DOCKER_UNIT.timer" "/etc/systemd/system/$DOCKER_UNIT.service" "$DOCKER_HELPER"
  rm -rf /run/clusterheart
}

if [ -n "$UNINSTALL" ]; then
  systemctl disable --now "$SERVICE" 2>/dev/null || true
  remove_docker_timer
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

# --update: newest agent and setup, same server id and token.
if [ -n "$UPDATE" ]; then
  if [ ! -f "$ENV_FILE" ]; then
    echo "The agent is not installed on this server. Use the install command from the dashboard." >&2
    exit 1
  fi
  ID="$(sed -n 's/^CLUSTER_ID=//p' "$ENV_FILE")"
  TOKEN="$(sed -n 's/^AGENT_TOKEN=//p' "$ENV_FILE")"
  CP_URL="$(sed -n 's/^CONTROLPLANE_URL=//p' "$ENV_FILE")"
fi

if [ -z "$ID" ] || [ -z "$TOKEN" ]; then
  echo "Usage: install-host.sh --id <server-id> --token <token>  (copy the command from the dashboard)" >&2
  exit 1
fi

download_agent

# Unprivileged user; the journal groups let it read system logs, video and
# render let it query GPUs where the driver restricts its device files.
if ! id clusterheart >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin clusterheart 2>/dev/null \
    || useradd -r -M -s /sbin/nologin clusterheart
fi
for g in systemd-journal adm video render; do
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

# Docker's socket is root-equivalent, so the agent never touches it. A small
# root timer runs one fixed `docker ps` and leaves only these fields in a file
# the agent can read: no commands, labels, mounts or environment.
if command -v docker >/dev/null 2>&1; then
  mkdir -p "$(dirname "$DOCKER_HELPER")"
  cat > "$DOCKER_HELPER" <<'EOF'
#!/bin/sh
set -eu
umask 027
d=/run/clusterheart
chown root:clusterheart "$d"
chmod 0750 "$d"
t="$(mktemp "$d/.docker.XXXXXX")"
trap 'rm -f "$t"' EXIT
docker ps -a --format '{"id":{{json .ID}},"name":{{json .Names}},"image":{{json .Image}},"state":{{json .State}},"status":{{json .Status}},"ports":{{json .Ports}}}' > "$t"
chown root:clusterheart "$t"
chmod 0640 "$t"
mv -f "$t" "$d/docker.json"
trap - EXIT
EOF
  chown root:root "$DOCKER_HELPER"
  chmod 0755 "$DOCKER_HELPER"

  cat > "/etc/systemd/system/$DOCKER_UNIT.service" <<EOF
[Unit]
Description=Clusterheart container list (docker ps, read-only)

[Service]
Type=oneshot
Group=clusterheart
ExecStart=$DOCKER_HELPER
TimeoutStartSec=20
Environment=DOCKER_CONFIG=/nonexistent
RuntimeDirectory=clusterheart
RuntimeDirectoryMode=0750
RuntimeDirectoryPreserve=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
EOF

  cat > "/etc/systemd/system/$DOCKER_UNIT.timer" <<EOF
[Unit]
Description=Refresh the Clusterheart container list

[Timer]
OnActiveSec=0
OnUnitActiveSec=30s
AccuracySec=5s

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable "$DOCKER_UNIT.timer" >/dev/null 2>&1
  systemctl restart "$DOCKER_UNIT.timer"
else
  remove_docker_timer
fi

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1
systemctl restart "$SERVICE"
if [ -n "$UPDATE" ]; then
  check_running "Clusterheart host agent updated and running."
else
  check_running "Clusterheart host agent is running. This server will show as Connected within a minute."
fi
