//go:build !linux

package handler

// Reaper is the process-wide status registry. On non-Linux builds nothing feeds
// it — the agent only ever runs inside the guest — but it must exist so the
// package compiles and the session-wait path is exercised by host-side tests.
var Reaper = newStatusRegistry()

// ReapForever is a no-op off Linux: there is no wait4 and no PID 1 role.
func ReapForever() {}
