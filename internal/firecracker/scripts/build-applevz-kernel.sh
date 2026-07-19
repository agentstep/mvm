#!/bin/bash
set -e
# Build a custom Linux kernel for the applevz (Apple Virtualization.framework)
# backend, with virtio-fs support added on top of Firecracker's own CI kernel
# config so that `-V hostPath:guestPath` volume mounts (virtio-fs) work.
#
# Firecracker's shared CI kernel (firecracker-ci/v1.13/aarch64/vmlinux-6.1.*,
# downloaded by build-rootfs.sh) has CONFIG_FUSE_FS=y but CONFIG_VIRTIO_FS unset,
# so `mount -t virtiofs` fails with "unknown filesystem type 'virtiofs'".
# This kernel starts from that exact CI config (which already has the correct
# virtio-net/blk/vsock/balloon setup for the raw-Image boot path both backends
# share) and enables CONFIG_VIRTIO_FS. Everything else is left as-is.
#
# The result is written to $CACHE_DIR/vmlinux-applevz — a SEPARATE file from the
# shared $CACHE_DIR/vmlinux, so the Firecracker backend's kernel is untouched.
#
# Env: CACHE_DIR (where vmlinux-applevz is written)
#      ARCH (aarch64; mapped to the kernel's arm64)
#      KERNEL_VERSION (default 6.1.141, to match the CI kernel this project ships)
#      FC_REF (Firecracker git ref to pull the base config from; default main)
#
# This script runs INSIDE the Lima VM (not on macOS), like build-rootfs.sh.
# On Apple Silicon the Lima VM is itself aarch64, so this is a native build
# (no CROSS_COMPILE toolchain needed).

CACHE_DIR="${CACHE_DIR:?CACHE_DIR must be set}"
ARCH="${ARCH:-aarch64}"
KERNEL_VERSION="${KERNEL_VERSION:-6.1.141}"
FC_REF="${FC_REF:-main}"

# Map the project's "aarch64" to the kernel build system's "arm64".
case "$ARCH" in
    aarch64|arm64) KARCH="arm64" ;;
    x86_64|amd64)  KARCH="x86_64" ;;
    *) echo "ERROR: unsupported ARCH '$ARCH'" >&2; exit 1 ;;
esac

# Kernel major series (6.1.141 -> 6.x) for the cdn.kernel.org path.
KMAJOR="$(echo "$KERNEL_VERSION" | cut -d. -f1)"
BUILD_DIR="/opt/mvm/kernel-build"
SRC_DIR="$BUILD_DIR/linux-$KERNEL_VERSION"
CONFIG_NAME="microvm-kernel-ci-${ARCH}-${KMAJOR}.1.config"
CONFIG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_REF}/resources/guest_configs/${CONFIG_NAME}"

trap 'sudo rm -rf "$BUILD_DIR"' EXIT

echo "Installing kernel build dependencies..."
sudo apt-get update -qq
sudo apt-get install -y build-essential bc flex bison libssl-dev libelf-dev wget xz-utils >/dev/null

sudo mkdir -p "$CACHE_DIR" "$BUILD_DIR"
sudo chown "$(id -u):$(id -g)" "$BUILD_DIR"

# --- Fetch Firecracker's CI kernel config (the base we extend) ---
echo "Fetching base config: $CONFIG_NAME (Firecracker ref $FC_REF)..."
wget -q "$CONFIG_URL" -O "$BUILD_DIR/base.config"

# --- Download and extract the matching kernel source ---
echo "Downloading Linux $KERNEL_VERSION source..."
wget -q "https://cdn.kernel.org/pub/linux/kernel/v${KMAJOR}.x/linux-${KERNEL_VERSION}.tar.xz" \
    -O "$BUILD_DIR/linux.tar.xz"
echo "Extracting..."
tar -C "$BUILD_DIR" -xf "$BUILD_DIR/linux.tar.xz"

cd "$SRC_DIR"
cp "$BUILD_DIR/base.config" .config

# --- Add virtio-fs (FUSE is already =y in the CI config, but be explicit) ---
echo "Enabling virtio-fs / FUSE..."
./scripts/config --enable CONFIG_FUSE_FS --enable CONFIG_VIRTIO_FS
make ARCH="$KARCH" olddefconfig

# Fail loudly if the options didn't stick (e.g. a missing dependency silently
# dropped them during olddefconfig).
if ! grep -q '^CONFIG_VIRTIO_FS=y' .config; then
    echo "ERROR: CONFIG_VIRTIO_FS did not end up enabled in .config" >&2
    exit 1
fi

# --- Build the raw kernel Image ---
echo "Building kernel Image (ARCH=$KARCH, -j$(nproc))..."
make ARCH="$KARCH" -j"$(nproc)" Image

# arm64 emits arch/arm64/boot/Image; x86_64 emits arch/x86/boot/bzImage.
if [ "$KARCH" = "arm64" ]; then
    IMAGE_PATH="arch/arm64/boot/Image"
else
    IMAGE_PATH="arch/x86/boot/bzImage"
fi

echo "Installing kernel to $CACHE_DIR/vmlinux-applevz..."
sudo cp "$IMAGE_PATH" "$CACHE_DIR/vmlinux-applevz"

sudo rm -rf "$BUILD_DIR"
trap - EXIT

echo "applevz kernel ready: $CACHE_DIR/vmlinux-applevz (Linux $KERNEL_VERSION + virtio-fs)"
