package server

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecureSocketNeverWorldAccessible is the invariant that matters. The unix
// handler has no authentication, so anyone who can open this socket can ask the
// daemon to run commands as root — the permission bits are the entire boundary.
func TestSecureSocketNeverWorldAccessible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.sock")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}

	if err := secureSocket(path); err != nil {
		t.Fatalf("secureSocket: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := fi.Mode().Perm()
	if mode&0o007 != 0 {
		t.Errorf("mode = %04o, must grant nothing to other", mode)
	}
	if mode&0o002 != 0 {
		t.Errorf("mode = %04o, must never be world-writable", mode)
	}
}

// TestSecureSocketStartsClosed pins that the restrictive mode is applied first,
// so any later failure leaves the socket closed rather than open.
func TestSecureSocketStartsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.sock")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := secureSocket(path); err != nil {
		t.Fatalf("secureSocket: %v", err)
	}
	fi, _ := os.Stat(path)
	// Running as an unprivileged user in tests, consoleOwner reports false, so
	// the socket should be left at the closed default.
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600 when there is no separate console user", got)
	}
}

func TestSecureSocketMissingFile(t *testing.T) {
	if err := secureSocket(filepath.Join(t.TempDir(), "nope.sock")); err == nil {
		t.Error("securing a socket that does not exist should error")
	}
}

// TestConsoleOwnerIgnoresRootHome — a root HOME means there is no unprivileged
// user to hand access to, so the socket must stay owner-only.
func TestConsoleOwnerIgnoresRootHome(t *testing.T) {
	t.Setenv(SocketOwnerEnv, "/root")
	if _, _, ok := consoleOwner(); ok {
		t.Error("a /root home must not be treated as a separate console user")
	}
}

func TestConsoleOwnerIgnoresMissingHome(t *testing.T) {
	t.Setenv(SocketOwnerEnv, filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("HOME", "")
	if _, _, ok := consoleOwner(); ok {
		t.Error("a home that does not exist must not yield an owner")
	}
}

// TestConsoleOwnerRequiresRoot — when the daemon is already the same user as
// the client there is nothing to hand over, so no chown should be attempted.
func TestConsoleOwnerRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test asserts the non-root path")
	}
	t.Setenv(SocketOwnerEnv, t.TempDir())
	if _, _, ok := consoleOwner(); ok {
		t.Error("a non-root daemon must not try to chown its socket")
	}
}

func TestSocketOwnerDescription(t *testing.T) {
	if got := SocketOwnerDescription(); got == "" {
		t.Error("description must not be empty")
	}
}
