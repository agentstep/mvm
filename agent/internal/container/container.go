// Package container manages the inner namespace that user code runs in.
//
// mvm-agent is PID 1 in the guest. Without this package it is also the direct
// parent of every user process, in the same namespace — so there is no boundary
// between the supervisor and what it supervises, and no way to restart user
// code without rebooting the VM.
//
// This package slides a namespace between the two: the agent stays in the root
// namespace, and user code runs in a child namespace whose PID 1 is the agent
// re-execing itself. See docs/superpowers/specs/2026-08-20-inner-container-design.md.
//
// IMPORTANT: this provides NO security isolation. The guest is root, there is no
// user namespace, the network namespace is shared, and the rootfs is shared. It
// is a lifecycle boundary only. Nothing in this package or its callers may imply
// otherwise.
package container

import "strings"

// InitFlag marks a process as the inner-namespace init. The agent re-execs
// itself with this argument; main() dispatches on it before doing anything else.
//
// Deliberately not a registered CLI flag: it is an implementation detail of the
// re-exec, never something a user passes.
const InitFlag = "--container-init"

// Namespace flags, defined here rather than taken from syscall so this file
// builds and is testable on any platform. Values are the stable Linux ABI.
const (
	flagNewNS   uintptr = 0x00020000 // CLONE_NEWNS   — mount
	flagNewUTS  uintptr = 0x04000000 // CLONE_NEWUTS  — hostname
	flagNewIPC  uintptr = 0x08000000 // CLONE_NEWIPC  — SysV IPC, POSIX mqueues
	flagNewUser uintptr = 0x10000000 // CLONE_NEWUSER — deliberately unused
	flagNewPid  uintptr = 0x20000000 // CLONE_NEWPID  — process IDs
	flagNewNet  uintptr = 0x40000000 // CLONE_NEWNET  — deliberately unused
)

// CloneFlags returns the namespaces the inner container is created with.
//
// Network is deliberately absent: sharing the guest's netns keeps the guest IP,
// the iptables DNAT port forwarding, the tcp_forward handler and in-guest
// network policy working without modification. It costs nothing in isolation
// terms, because the agent's control channel is vsock, which carries no IP.
//
// User is deliberately absent: the guest is root-only by design, CLONE_NEWUSER
// is the namespace most likely to be unavailable, and it would complicate every
// uid-sensitive path for no gain.
func CloneFlags() uintptr {
	return flagNewPid | flagNewNS | flagNewIPC | flagNewUTS
}

// Describe lists the namespaces the inner container unshares, for logs and
// diagnostics. Only the ones actually created are named — see CloneFlags for
// why network and user are not among them.
func Describe() string {
	return "pid, mnt, ipc, uts"
}

// InitCommand returns the path and arguments used to re-exec this binary as the
// inner init.
//
// The path is /proc/self/exe, not the on-disk agent path. The magic link pins
// the running inode, so a binary upgrade — or a respawn after the file changed
// underneath — cannot produce an inner init speaking a different protocol
// version than the outer agent that spawned it.
func InitCommand() (path string, args []string) {
	return "/proc/self/exe", []string{InitFlag}
}

// IsInitProcess reports whether this process was started as the inner init.
// Pass os.Args.
func IsInitProcess(argv []string) bool {
	for _, a := range argv[min(1, len(argv)):] {
		if strings.TrimSpace(a) == InitFlag {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
