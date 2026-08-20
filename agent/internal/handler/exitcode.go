package handler

import (
	"errors"
	"os/exec"
	"syscall"
)

// ExitWaitFailed is the exit code reported when a command ran but its status
// could not be collected. It is deliberately not 0 and not a plausible real
// exit code from a shell (which uses 1-125 for command failures, 126/127 for
// exec problems, 128+n for signals).
//
// Reporting 0 here is the bug this constant exists to prevent: an exit frame
// saying "success" for a command whose outcome is unknown is worse than one
// saying "something went wrong", because callers branch on it.
const ExitWaitFailed = 255

// ExitCodeFrom converts a cmd.Wait()/cmd.Run() error into an exit code and an
// optional diagnostic.
//
// A nil error is exit 0. An *exec.ExitError carries the real code. Anything
// else means the wait itself failed, and must NOT be reported as success.
//
// The case that matters is ECHILD. When something else has already reaped the
// child — a SIGCHLD reaper, or PID 1 in a namespace where this process is not
// the parent — Wait() cannot find the process and fails with ECHILD rather
// than an ExitError. Callers that have a status registry should consult it
// before falling back here; see the inner-container design, which makes this
// path common rather than rare.
// WaitSession waits for an already-started command, tolerating a concurrent
// reaper.
//
// Sessions must use this rather than calling cmd.Wait() directly. With a
// wait4(-1) reaper running, cmd.Wait() sometimes loses the race and returns
// ECHILD with no exit code; the status is not lost, it went to the registry, so
// this collects it from there instead.
//
// The pid is registered before Wait() so a fast-exiting child cannot slip
// through the gap between the two (statusRegistry.pending covers the remaining
// window between Start() and this call).
func WaitSession(cmd *exec.Cmd) (code int, diagnostic string) {
	if cmd.Process == nil {
		return ExitWaitFailed, "wait failed: process was never started"
	}
	pid := cmd.Process.Pid
	ch := Reaper.register(pid)
	defer Reaper.unregister(pid)

	err := cmd.Wait()
	if err != nil && errors.Is(err, syscall.ECHILD) {
		select {
		case status := <-ch:
			return status, ""
		default:
			// Reaper has not delivered it and never will — nothing else can
			// produce this status now.
			return ExitWaitFailed, "exit status unavailable: child already reaped (ECHILD)"
		}
	}
	return ExitCodeFrom(err)
}

func ExitCodeFrom(err error) (code int, diagnostic string) {
	if err == nil {
		return 0, ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), ""
	}
	if errors.Is(err, syscall.ECHILD) {
		return ExitWaitFailed, "exit status unavailable: child already reaped (ECHILD)"
	}
	return ExitWaitFailed, "wait failed: " + err.Error()
}
