package handler

import (
	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandlePoweroff shuts the guest down.
//
// This used to shell out to a `poweroff` binary, with a comment noting it
// "works on Alpine". The rootfs is Debian bookworm with no systemd and no
// sysvinit, so no such binary is installed; the call was also `.Start()` with
// the error discarded, so it failed silently every time. `mvm stop` appeared to
// work only because StopViaAgent falls back to `kill -9` on the VMM after a
// timeout (internal/firecracker/process.go), and applevz stops via the VZ
// helper — so the graceful path has never actually been graceful.
//
// Even a working `poweroff` binary would not have helped: it signals init, and
// init here is mvm-agent, which installs no handler for it.
//
// The agent is PID 1, so it does the shutdown itself: flush filesystem buffers,
// then ask the kernel to power off. reboot(2) requires PID 1 (or CAP_SYS_BOOT,
// which root has); it does not return on success.
//
// NOTE for the inner-container work: this handler must remain in the ROOT
// namespace. reboot(2) called from a non-initial PID namespace terminates that
// namespace rather than the machine, which would make `mvm stop` report success
// while merely killing the container.
func HandlePoweroff() *protocol.Response {
	if err := powerOff(); err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}
	return &protocol.Response{Type: protocol.RespOK}
}
