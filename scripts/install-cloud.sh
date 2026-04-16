#!/bin/bash
set -euo pipefail

# mvm cloud install script
# Installs mvm daemon on a bare-metal Linux server with KVM support.

# --- Preflight checks ---

[ "$(id -u)" -ne 0 ] && echo "Run as root" && exit 1

[ ! -c /dev/kvm ] && echo "Error: /dev/kvm not found. Firecracker requires bare-metal Linux with KVM." && exit 1

# --- Detect architecture ---

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|aarch64) ;;
  *) echo "Error: unsupported architecture: $ARCH (need x86_64 or aarch64)" && exit 1 ;;
esac

echo "Installing mvm daemon for $ARCH..."

# --- Download binaries ---

curl -sSL -o /usr/local/bin/mvm \
  "https://github.com/paulmeller/mvm/releases/latest/download/mvm-linux-${ARCH}"

curl -sSL -o /usr/local/bin/firecracker \
  "https://github.com/paulmeller/mvm/releases/latest/download/firecracker-linux-${ARCH}"

chmod +x /usr/local/bin/mvm /usr/local/bin/firecracker

# --- Create directories ---

mkdir -p /var/mvm/{cache,vms,keys,pool}
mkdir -p /etc/mvm
mkdir -p /run/mvm

# --- Generate self-signed TLS certificate ---

openssl req -x509 -newkey rsa:4096 \
  -keyout /etc/mvm/key.pem -out /etc/mvm/cert.pem \
  -days 365 -nodes -subj "/CN=mvm-daemon"
chmod 600 /etc/mvm/key.pem

# --- Generate random API key ---

openssl rand -hex 32 > /etc/mvm/api-key
chmod 600 /etc/mvm/api-key

# --- Install systemd unit ---

cat > /etc/systemd/system/mvm-daemon.service <<'EOF'
[Unit]
Description=mvm sandbox daemon
Documentation=https://github.com/paulmeller/mvm
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/mvm serve start --listen 0.0.0.0:19876 --tls-cert /etc/mvm/cert.pem --tls-key /etc/mvm/key.pem --api-key-file /etc/mvm/api-key
Environment=MVM_DATA_DIR=/var/mvm
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now mvm-daemon

# --- Print summary ---

cat <<SUMMARY

mvm sandbox daemon installed!

API key:  $(cat /etc/mvm/api-key)
Endpoint: https://$(hostname):19876

From your laptop:
  export MVM_REMOTE=https://$(hostname):19876
  export MVM_API_KEY=$(cat /etc/mvm/api-key)
  mvm pool status

SUMMARY
