//go:build linux

package handler

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// Reaper is the process-wide status registry. Sessions register their child pid
// here; ReapForever delivers statuses to them.
var Reaper = newStatusRegistry()

// ReapForever collects exited children for the lifetime of the process.
//
// mvm-agent runs as PID 1, so every orphaned process in the guest reparents to
// it. Until this existed the agent never called wait4 at all, so those orphans
// became permanent zombies — one for every `mvm exec -d` job, accumulating for
// the life of the VM.
//
// Statuses are routed through Reaper rather than collected by each session's
// own cmd.Wait(), because the two race: whichever gets there first wins, and a
// session that loses sees ECHILD instead of its exit code. See statusRegistry.
//
// Call once, in a goroutine, before serving any connection.
func ReapForever() {
	// SIGCHLD is ignored by default and Go installs no handler for it, so ask
	// for delivery explicitly. The channel is only a wakeup: the wait4 loop
	// below drains every available child each time, so a coalesced signal (the
	// kernel does not queue duplicates) still reaps all of them.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)

	for range ch {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				continue
			}
			// pid 0: children exist but none have exited. err ECHILD: no
			// children at all. Either way, wait for the next SIGCHLD.
			if pid <= 0 || err != nil {
				break
			}
			Reaper.deliver(pid, waitStatusToExitCode(ws))
		}
	}
}

// waitStatusToExitCode renders a wait status the way a shell would: the exit
// code for a normal exit, 128+signal for a signal death. This keeps `mvm exec`
// exit codes consistent whether the status came from cmd.Wait() or from here.
func waitStatusToExitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// LogReaperUnavailable records why reaping is off on platforms without wait4.
func LogReaperUnavailable() { log.Printf("reaper unavailable on this platform") }
