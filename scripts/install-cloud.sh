#!/bin/bash
set -euo pipefail

# mvm cloud install script
# Installs mvm daemon on a bare-metal Linux server with KVM support.
# Self-contained — bootstraps everything needed to run microVMs:
#   - binaries (mvm, firecracker, mvm-agent)
#   - TLS certificate + API key
#   - systemd unit for the daemon
#   - Debian rootfs + kernel in /var/mvm/cache
#   - NAT / IP forwarding for VM egress

# --- Preflight checks ---

[ "$(id -u)" -ne 0 ] && echo "Run as root" && exit 1

[ ! -c /dev/kvm ] && echo "Error: /dev/kvm not found. Firecracker requires bare-metal Linux with KVM." && exit 1

# --- Detect architecture ---

ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  DEB_ARCH=amd64 ;;
  aarch64) DEB_ARCH=arm64 ;;
  *) echo "Error: unsupported architecture: $ARCH (need x86_64 or aarch64)" && exit 1 ;;
esac

echo "=== Installing mvm daemon for $ARCH ==="

# --- Install host dependencies ---

echo "=== Installing host dependencies ==="
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends \
    curl wget openssl ca-certificates \
    debootstrap e2fsprogs \
    iptables iproute2 \
    netcat-openbsd \
    >/dev/null

# --- Download binaries ---

echo "=== Downloading binaries ==="

curl -fsSL -o /usr/local/bin/mvm \
  "https://github.com/paulmeller/mvm/releases/latest/download/mvm-linux-${ARCH}"

curl -fsSL -o /usr/local/bin/firecracker \
  "https://github.com/paulmeller/mvm/releases/latest/download/firecracker-linux-${ARCH}"

curl -fsSL -o /usr/local/bin/mvm-agent \
  "https://github.com/paulmeller/mvm/releases/latest/download/mvm-agent-linux-${ARCH}"

curl -fsSL -o /usr/local/bin/mvm-uffd \
  "https://github.com/paulmeller/mvm/releases/latest/download/mvm-uffd-linux-${ARCH}"

chmod +x /usr/local/bin/mvm /usr/local/bin/firecracker /usr/local/bin/mvm-agent /usr/local/bin/mvm-uffd

# --- Create directories ---

mkdir -p /var/mvm/{cache,vms,keys,pool}
mkdir -p /etc/mvm
mkdir -p /run/mvm

# --- Generate self-signed TLS certificate ---

if [ ! -f /etc/mvm/cert.pem ] || [ ! -f /etc/mvm/key.pem ]; then
    openssl req -x509 -newkey rsa:4096 \
      -keyout /etc/mvm/key.pem -out /etc/mvm/cert.pem \
      -days 365 -nodes -subj "/CN=mvm-daemon"
    chmod 600 /etc/mvm/key.pem
fi

# --- Generate random API key ---

if [ ! -f /etc/mvm/api-key ]; then
    openssl rand -hex 32 > /etc/mvm/api-key
    chmod 600 /etc/mvm/api-key
fi

# --- Build Debian rootfs (idempotent) ---

if [ -f /var/mvm/cache/base.ext4 ] && [ -f /var/mvm/cache/vmlinux ]; then
    echo "=== Rootfs already built, skipping ==="
else
    echo "=== Building Debian rootfs ==="

    # Write the build scripts to a temp dir. These are embedded verbatim from
    # internal/firecracker/scripts/ so this install script stays self-contained.
    BUILD_DIR=$(mktemp -d)
    trap 'rm -rf "$BUILD_DIR"' EXIT

    cat > "$BUILD_DIR/chroot-setup.sh" << 'CHROOT_SETUP_EOF'
#!/bin/bash
set -e
# Configure Debian rootfs for Firecracker microVMs.
# Runs INSIDE the chroot — no quoting layers, no nesting issues.
# Env: MINIMAL (0 or 1)

# --- Packages ---
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends \
    git curl wget python3 python3-pip \
    nodejs npm \
    iptables iproute2 ripgrep \
    busybox-static ca-certificates procps \
    >/dev/null 2>&1

# busybox init — symlink so kernel finds it at /sbin/init
ln -sf /bin/busybox /sbin/init

# --- Hostname + DNS ---
echo "mvm" > /etc/hostname
echo "nameserver 8.8.8.8" > /etc/resolv.conf
echo "root:root" | chpasswd 2>/dev/null || true

# --- PATH + V8 cache ---
mkdir -p /etc/profile.d
echo 'export PATH=/usr/local/bin:/root/.local/bin:$PATH' >> /etc/profile
cat > /etc/profile.d/mvm-env.sh << 'ENVEOF'
export PATH=/usr/local/bin:/root/.local/bin:$PATH
export NODE_COMPILE_CACHE=/tmp/v8-cache
ENVEOF

# --- Minimal busybox inittab (no systemd, ~2s boot) ---
cat > /etc/inittab << 'INITTAB'
::sysinit:/bin/mkdir -p /proc /sys /dev /tmp /run /dev/pts /dev/shm /tmp/v8-cache
::sysinit:/bin/mount -t proc proc /proc
::sysinit:/bin/mount -t sysfs sys /sys
::sysinit:/bin/mount -t devtmpfs dev /dev
::sysinit:/bin/mount -t tmpfs tmpfs /tmp
::sysinit:/bin/mount -t tmpfs tmpfs /run
::sysinit:/bin/mount -t devpts devpts /dev/pts
::sysinit:/bin/mount -t tmpfs tmpfs /dev/shm
::sysinit:/bin/hostname mvm
::sysinit:/etc/init.d/mvm-init
ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100
::shutdown:/bin/kill -TERM -1
::shutdown:/bin/umount -a -r
INITTAB

# --- Init script: starts agent + network watchdog ---
mkdir -p /etc/init.d
cat > /etc/init.d/mvm-init << 'INITSCRIPT'
#!/bin/sh
mount -o remount,rw / 2>/dev/null
# Start vsock agent (also listens on TCP :5123)
if [ -x /opt/mvm-agent ]; then
    /opt/mvm-agent &
    echo $! > /run/mvm-agent.pid
fi
# Network watchdog for snapshot restore
/etc/local.d/network-watchdog.start &
INITSCRIPT
chmod +x /etc/init.d/mvm-init

# --- Network watchdog ---
mkdir -p /etc/local.d
cat > /etc/local.d/network-watchdog.start << 'WATCHDOG'
#!/bin/sh
while true; do
    sleep 2
    if [ -f /sys/class/net/eth0/carrier ]; then
        carrier=$(cat /sys/class/net/eth0/carrier 2>/dev/null || echo 0)
        if [ "$carrier" = "0" ]; then
            ip link set eth0 down 2>/dev/null
            sleep 0.3
            ip link set eth0 up 2>/dev/null
            gw=$(sed 's/.*ip=[^:]*::\([^:]*\):.*/\1/' /proc/cmdline)
            if [ -n "$gw" ] && [ "$gw" != "$(cat /proc/cmdline)" ]; then
                ip route add default via "$gw" dev eth0 2>/dev/null
                echo "nameserver 8.8.8.8" > /etc/resolv.conf
            fi
        fi
    fi
done
WATCHDOG
chmod +x /etc/local.d/network-watchdog.start

# --- Install AI agents (unless minimal) ---
if [ "$MINIMAL" != "1" ]; then
    # Claude Code native binary (Bun-based, ~2s startup on glibc)
    echo "Installing Claude Code (native)..."
    curl -fsSL https://claude.ai/install.sh | bash 2>/dev/null || true

    # Codex CLI (native Rust binary)
    echo "Installing Codex CLI (native)..."
    curl -fsSL https://github.com/openai/codex/releases/latest/download/install.sh | sh 2>/dev/null || true
fi

# --- SKILLS.md ---
mkdir -p /.mvm /root/.ssh
chmod 700 /root/.ssh
cat > /.mvm/SKILLS.md << 'SKILLS'
# MVM Environment

You are inside an mvm microVM — a hardware-isolated Firecracker VM.

## Pre-installed
- Node.js (node, npm), Python 3 (python3, pip)
- git, curl, wget, ripgrep
- Claude Code (claude), Codex (codex)
- Base OS: Debian Bookworm

## Package management
- Install packages: apt-get install -y <package>

## Tips
- Writable root filesystem — install anything you need
- State persists until VM is deleted
SKILLS
cp /.mvm/SKILLS.md /root/CLAUDE.md
cp /.mvm/SKILLS.md /root/AGENTS.md

# --- Cleanup to reduce image size ---
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/chroot-setup.sh

echo "Chroot setup complete"
CHROOT_SETUP_EOF

    cat > "$BUILD_DIR/build-rootfs.sh" << 'BUILD_ROOTFS_EOF'
#!/bin/bash
set -e
# Build a Debian rootfs for Firecracker microVMs.
# Env: CACHE_DIR, ARCH (kernel arch: x86_64|aarch64), DEB_ARCH (debootstrap arch: amd64|arm64),
#      MINIMAL, AGENT_BIN, CHROOT_SETUP (path to chroot-setup.sh)

ROOTFS_DIR="/opt/mvm/rootfs-build"

trap 'rm -rf "$ROOTFS_DIR"' EXIT

mkdir -p "$CACHE_DIR"

# --- Download kernel ---
echo "Downloading kernel..."
latest_kernel_key=$(wget -q "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/v1.13/${ARCH}/vmlinux-5.10&list-type=2" -O - \
    | sed -n "s/.*<Key>\(firecracker-ci\/v1.13\/${ARCH}\/vmlinux-5\.10\.[0-9]*\)<\/Key>.*/\1/p" \
    | sort -V | tail -1)
if [ -z "$latest_kernel_key" ]; then
    echo "ERROR: Could not find kernel image" >&2
    exit 1
fi
wget -q "https://s3.amazonaws.com/spec.ccfc.min/${latest_kernel_key}" -O "$CACHE_DIR/vmlinux"
echo "Kernel downloaded: $(basename "$latest_kernel_key")"

# --- Build Debian rootfs via debootstrap ---
echo "Building Debian rootfs (${DEB_ARCH})..."
rm -rf "$ROOTFS_DIR"
mkdir -p "$(dirname "$ROOTFS_DIR")"

debootstrap --variant=minbase --arch="$DEB_ARCH" bookworm "$ROOTFS_DIR" http://deb.debian.org/debian
echo "Debian base installed"

# --- Run chroot setup ---
cp "$CHROOT_SETUP" "$ROOTFS_DIR/tmp/chroot-setup.sh"
chmod +x "$ROOTFS_DIR/tmp/chroot-setup.sh"
chroot "$ROOTFS_DIR" env MINIMAL="$MINIMAL" /bin/bash /tmp/chroot-setup.sh

# --- Inject agent binary ---
if [ -n "$AGENT_BIN" ] && [ -f "$AGENT_BIN" ]; then
    mkdir -p "$ROOTFS_DIR/opt"
    cp "$AGENT_BIN" "$ROOTFS_DIR/opt/mvm-agent"
    chmod +x "$ROOTFS_DIR/opt/mvm-agent"
    echo "Guest agent injected"
else
    echo "WARNING: No agent binary at $AGENT_BIN — VMs will have no agent" >&2
fi

# --- Symlink binaries into /usr/bin ---
chroot "$ROOTFS_DIR" /bin/bash -c 'for bin in /usr/local/bin/*; do [ -x "$bin" ] && [ ! -e "/usr/bin/$(basename "$bin")" ] && ln -sf "$bin" "/usr/bin/$(basename "$bin")" 2>/dev/null || true; done'
chroot "$ROOTFS_DIR" /bin/bash -c 'for bin in /root/.local/bin/*; do [ -x "$bin" ] && ln -sf "$bin" "/usr/bin/$(basename "$bin")" 2>/dev/null || true; done' 2>/dev/null || true

# --- Create ext4 image ---
if [ "$MINIMAL" = "1" ]; then
    IMG_SIZE=1024
else
    IMG_SIZE=2048
fi
echo "Creating ext4 image (${IMG_SIZE}MB)..."
dd if=/dev/zero of="$CACHE_DIR/base.ext4" bs=1M count=0 seek=$IMG_SIZE status=none
mkfs.ext4 -F -d "$ROOTFS_DIR" "$CACHE_DIR/base.ext4" >/dev/null 2>&1

# --- Cleanup ---
rm -rf "$ROOTFS_DIR"
trap - EXIT

echo "Debian rootfs ready: $CACHE_DIR/base.ext4"
BUILD_ROOTFS_EOF

    chmod +x "$BUILD_DIR"/*.sh

    # Run as root (we are already root per preflight check) with env vars.
    # MINIMAL=0 installs AI agents; set MVM_MINIMAL=1 to skip.
    CACHE_DIR=/var/mvm/cache \
    ARCH="$ARCH" \
    DEB_ARCH="$DEB_ARCH" \
    MINIMAL="${MVM_MINIMAL:-0}" \
    AGENT_BIN=/usr/local/bin/mvm-agent \
    CHROOT_SETUP="$BUILD_DIR/chroot-setup.sh" \
    "$BUILD_DIR/build-rootfs.sh"

    rm -rf "$BUILD_DIR"
    trap - EXIT
fi

# --- Configure NAT so VMs can reach the internet ---

echo "=== Configuring NAT ==="

# Enable IP forwarding
sysctl -w net.ipv4.ip_forward=1 >/dev/null
mkdir -p /etc/sysctl.d
echo "net.ipv4.ip_forward=1" > /etc/sysctl.d/99-mvm.conf

# Detect default network interface
HOST_IFACE=$(ip route list default | awk '{print $5; exit}')
if [ -z "$HOST_IFACE" ]; then
    echo "ERROR: Could not detect default network interface" >&2
    exit 1
fi

# MASQUERADE for microVM traffic (mvm uses 172.16.0.0/24 in /30 chunks per VM)
iptables -t nat -C POSTROUTING -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -o "$HOST_IFACE" -j MASQUERADE
iptables -P FORWARD ACCEPT

# Persist NAT rules across reboots via a systemd oneshot
cat > /usr/local/bin/mvm-nat.sh << 'NAT_EOF'
#!/bin/bash
set -e
sysctl -w net.ipv4.ip_forward=1
HOST_IFACE=$(ip route list default | awk '{print $5; exit}')
if [ -z "$HOST_IFACE" ]; then
    echo "ERROR: Could not detect default network interface" >&2
    exit 1
fi
iptables -t nat -C POSTROUTING -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -o "$HOST_IFACE" -j MASQUERADE
iptables -P FORWARD ACCEPT
echo "NAT configured on $HOST_IFACE"
NAT_EOF
chmod +x /usr/local/bin/mvm-nat.sh

cat > /etc/systemd/system/mvm-nat.service << 'UNIT_EOF'
[Unit]
Description=MVM NAT forwarding rules
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/mvm-nat.sh

[Install]
WantedBy=multi-user.target
UNIT_EOF

systemctl daemon-reload
systemctl enable mvm-nat.service >/dev/null 2>&1

# --- Install systemd unit for the daemon ---

cat > /etc/systemd/system/mvm-daemon.service <<'EOF'
[Unit]
Description=mvm sandbox daemon
Documentation=https://github.com/paulmeller/mvm
After=network.target mvm-nat.service
Wants=mvm-nat.service

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

=== mvm sandbox daemon installed! ===

API key:  $(cat /etc/mvm/api-key)
Endpoint: https://$(hostname):19876

Rootfs:   /var/mvm/cache/base.ext4
Kernel:   /var/mvm/cache/vmlinux

From your laptop:
  export MVM_REMOTE=https://$(hostname):19876
  export MVM_API_KEY=$(cat /etc/mvm/api-key)
  mvm pool status

SUMMARY
