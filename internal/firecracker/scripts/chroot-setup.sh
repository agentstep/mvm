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

You are inside an mvm microVM — a hardware-isolated Firecracker VM on macOS.

## Pre-installed
- Node.js (node, npm), Python 3 (python3, pip)
- git, curl, wget, ripgrep
- Claude Code (claude), Codex (codex)
- Base OS: Debian Bookworm (aarch64)

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
