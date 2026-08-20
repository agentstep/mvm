package server

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// SocketOwnerEnv names the account the control socket should be readable by.
// The daemon's systemd unit sets it; nothing else needs to.
const SocketOwnerEnv = "MVM_SOCKET_OWNER_HOME"

// secureSocket restricts the control socket to the daemon and, when the daemon
// is running as root on behalf of an unprivileged console user, that user.
//
// The socket carries no authentication of its own: whoever can connect can ask
// the daemon to run commands as root. Its permissions are the entire boundary,
// so this never widens beyond a single named account.
//
// The awkward case is Lima. The daemon runs as root under systemd, but the SSH
// socket-forwarder that carries the socket out to macOS runs as the
// unprivileged Lima user, so a root-owned 0600 socket is simply unreachable
// from the CLI — which then reports "daemon not running" against a healthy
// daemon. Handing group access to exactly that one user fixes it without
// making the socket world-accessible.
//
// The owner is identified by a home directory (set by the unit via
// MVM_SOCKET_OWNER_HOME, falling back to HOME), because a directory's owner is
// something the daemon can stat without a users database or CGO.
func secureSocket(path string) error {
	// Start closed. If anything below fails, this is what remains in force.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod control socket: %w", err)
	}

	uid, gid, ok := consoleOwner()
	if !ok {
		// Daemon and client are the same user (the usual non-systemd case), so
		// owner-only access is already correct.
		return nil
	}

	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown control socket to uid %d: %w", uid, err)
	}
	// 0660 rather than 0600: the owning user reaches it directly, their group
	// covers the forwarder, and everyone else is still excluded.
	if err := os.Chmod(path, 0o660); err != nil {
		return fmt.Errorf("chmod control socket: %w", err)
	}
	return nil
}

// consoleOwner returns the uid/gid the control socket should belong to, and
// whether one was found.
//
// Reports false when the daemon is not root (nothing to hand over) or when the
// candidate home belongs to root anyway (no unprivileged user in the picture).
func consoleOwner() (uid, gid int, ok bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	home := os.Getenv(SocketOwnerEnv)
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" || home == "/root" {
		return 0, 0, false
	}
	fi, err := os.Stat(home)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// SocketOwnerDescription renders the resolved owner for diagnostics.
func SocketOwnerDescription() string {
	uid, gid, ok := consoleOwner()
	if !ok {
		return "daemon user only (0600)"
	}
	return "uid " + strconv.Itoa(uid) + ", gid " + strconv.Itoa(gid) + " (0660)"
}
