//go:build linux

package handler

import "syscall"

// powerOff flushes filesystem buffers and asks the kernel to power the machine
// off. It does not return on success.
//
// Sync() first: reboot(RB_POWER_OFF) does not flush page cache, so without it a
// guest that has just written files can lose them — which matters here because
// the rootfs persists across stop/start and is the VM's durable state.
func powerOff() error {
	syscall.Sync()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}
