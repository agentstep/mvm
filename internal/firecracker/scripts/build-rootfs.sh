#!/bin/bash
set -e
# Build an Alpine rootfs for Firecracker microVMs.
# Env: CACHE_DIR, ARCH, MINIMAL, AGENT_BIN (path to mvm-agent binary in Lima)
#
# This script runs INSIDE the Lima VM (not on macOS).

ROOTFS_DIR="/opt/mvm/rootfs-build"
ALPINE_VERSION="3.21"

trap 'sudo rm -rf "$ROOTFS_DIR" /tmp/alpine.tar.gz /tmp/chroot-setup.sh' EXIT

sudo mkdir -p "$CACHE_DIR"

# --- Download kernel ---
echo "Downloading kernel..."
latest_kernel_key=$(wget -q "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/v1.13/${ARCH}/vmlinux-5.10&list-type=2" -O - \
    | sed -n "s/.*<Key>\(firecracker-ci\/v1.13\/${ARCH}\/vmlinux-5\.10\.[0-9]*\)<\/Key>.*/\1/p" \
    | sort -V | tail -1)
if [ -z "$latest_kernel_key" ]; then
    echo "ERROR: Could not find kernel image" >&2
    exit 1
fi
sudo wget -q "https://s3.amazonaws.com/spec.ccfc.min/${latest_kernel_key}" -O "$CACHE_DIR/vmlinux"
echo "Kernel downloaded: $(basename "$latest_kernel_key")"

# --- Download and extract Alpine ---
echo "Building Alpine rootfs..."
sudo rm -rf "$ROOTFS_DIR"
wget -q "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/releases/aarch64/alpine-minirootfs-${ALPINE_VERSION}.0-aarch64.tar.gz" -O /tmp/alpine.tar.gz
sudo mkdir -p "$ROOTFS_DIR"
sudo tar xzf /tmp/alpine.tar.gz -C "$ROOTFS_DIR"
sudo cp /etc/resolv.conf "$ROOTFS_DIR/etc/resolv.conf"

# --- Run chroot setup ---
# Copy the chroot script into the rootfs and run it as a file (not via sh -c).
# This avoids all heredoc/quoting issues.
sudo cp /tmp/chroot-setup.sh "$ROOTFS_DIR/tmp/chroot-setup.sh"
sudo chmod +x "$ROOTFS_DIR/tmp/chroot-setup.sh"
sudo chroot "$ROOTFS_DIR" env MINIMAL="$MINIMAL" /bin/sh /tmp/chroot-setup.sh

# --- Inject agent binary ---
if [ -n "$AGENT_BIN" ] && [ -f "$AGENT_BIN" ]; then
    sudo cp "$AGENT_BIN" "$ROOTFS_DIR/opt/mvm-agent"
    sudo chmod +x "$ROOTFS_DIR/opt/mvm-agent"
    echo "Guest agent injected"
else
    echo "WARNING: No agent binary at $AGENT_BIN — VMs will have no agent" >&2
fi

# --- Symlink /usr/local/bin into /usr/bin ---
sudo chroot "$ROOTFS_DIR" /bin/sh -c 'for bin in /usr/local/bin/*; do [ -x "$bin" ] && [ ! -e "/usr/bin/$(basename "$bin")" ] && ln -sf "$bin" "/usr/bin/$(basename "$bin")" 2>/dev/null || true; done'
# Also symlink ~/.local/bin (Codex installs here)
sudo chroot "$ROOTFS_DIR" /bin/sh -c 'for bin in /root/.local/bin/*; do [ -x "$bin" ] && ln -sf "$bin" "/usr/bin/$(basename "$bin")" 2>/dev/null || true; done' 2>/dev/null || true

# --- Create ext4 image ---
if [ "$MINIMAL" = "1" ]; then
    IMG_SIZE=1024
else
    IMG_SIZE=2048
fi
echo "Creating ext4 image (${IMG_SIZE}MB)..."
sudo dd if=/dev/zero of="$CACHE_DIR/base.ext4" bs=1M count=0 seek=$IMG_SIZE status=none
sudo mkfs.ext4 -F -d "$ROOTFS_DIR" "$CACHE_DIR/base.ext4" >/dev/null 2>&1

# --- Cleanup ---
sudo rm -rf "$ROOTFS_DIR" /tmp/alpine.tar.gz /tmp/chroot-setup.sh
trap - EXIT

echo "Alpine rootfs ready: $CACHE_DIR/base.ext4"
