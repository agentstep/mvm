package firecracker

import (
	"fmt"
	"strings"

	"github.com/agentstep/mvm/internal/lima"
)

// ChrootExec runs a command inside a VM's rootfs via chroot in Lima.
// The rootfs must NOT be in use by a running Firecracker process.
// This runs at native Lima speed (~10x faster than inside nested virt).
func ChrootExec(limaClient *lima.Client, rootfsPath, command string) error {
	// Escape rootfsPath for shell safety (prevent injection via VM names)
	safeRootfs := shellQuoteForChroot(rootfsPath)
	safeCommand := shellQuoteForChroot(command)

	script := fmt.Sprintf(`#!/bin/bash

ROOTFS=%s
MNT="/opt/mvm/chroot-mnt-$$"

# Cleanup function — called on exit, error, or Ctrl-C
cleanup() {
    sudo umount "$MNT/tmp" 2>/dev/null || true
    sudo umount "$MNT/sys" 2>/dev/null || true
    sudo umount "$MNT/dev" 2>/dev/null || true
    sudo umount "$MNT/proc" 2>/dev/null || true
    sudo umount "$MNT" 2>/dev/null || true
    sudo rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

# Optional fsck for dirty filesystems (crash recovery)
sudo e2fsck -y "$ROOTFS" >/dev/null 2>&1 || true

# Mount rootfs
sudo mkdir -p "$MNT"
sudo mount -o loop "$ROOTFS" "$MNT"

# Bind-mount system directories needed by package managers
sudo mount --bind /proc "$MNT/proc"
sudo mount --bind /dev "$MNT/dev"
sudo mount --bind /sys "$MNT/sys"
sudo mount -t tmpfs tmpfs "$MNT/tmp"

# Ensure DNS works inside chroot
sudo cp /etc/resolv.conf "$MNT/etc/resolv.conf" 2>/dev/null || true

# Run command in chroot (no set -e — we capture the exit code ourselves)
sudo chroot "$MNT" /bin/bash -c %s
exit $?
`, safeRootfs, safeCommand)

	_, err := limaClient.ShellScriptWithTimeout(script, lima.LongTimeout*2)
	return err
}

func shellQuoteForChroot(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
