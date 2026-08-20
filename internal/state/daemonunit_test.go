package state

import (
	"strings"
	"testing"
)

// TestDaemonUnitInvokesTheCurrentCommand is the regression guard on a failure
// that already happened: the installed unit kept invoking `mvm serve` after the
// CLI renamed it to `mvm system start`, so the daemon crash-looped while the
// binary on disk was fine. A stale unit and a broken daemon look identical from
// outside, so nothing noticed.
func TestDaemonUnitInvokesTheCurrentCommand(t *testing.T) {
	unit := DaemonUnit("/home/lima")
	if !strings.Contains(unit, "ExecStart="+DaemonBinaryPath+" system start") {
		t.Errorf("unit does not launch the daemon with `system start`:\n%s", unit)
	}
	if strings.Contains(unit, " serve") {
		t.Errorf("unit still references the removed `serve` command:\n%s", unit)
	}
}

// TestDaemonUnitPassesSocketOwner pins the other half of the Lima fix: the
// daemon runs as root but the socket-forwarder does not, so the daemon has to
// be told whose account to hand the control socket to.
func TestDaemonUnitPassesSocketOwner(t *testing.T) {
	unit := DaemonUnit("/home/lima.linux")
	if !strings.Contains(unit, "Environment=MVM_SOCKET_OWNER_HOME=/home/lima.linux") {
		t.Errorf("unit must pass the socket owner home:\n%s", unit)
	}
}

// TestInstallStopsBeforeReplacing — overwriting a running executable fails with
// ETXTBSY, and the failure is easy to miss because the old daemon keeps serving
// afterwards, so the deploy looks like it worked.
func TestInstallStopsBeforeReplacing(t *testing.T) {
	script := InstallDaemonUnitScript("/home/lima")
	stop := strings.Index(script, "systemctl stop mvm-daemon")
	write := strings.Index(script, DaemonUnitPath)
	if stop < 0 {
		t.Fatal("install must stop the daemon before touching its files")
	}
	if write < stop {
		t.Error("the unit is written before the daemon is stopped")
	}
	if !strings.Contains(script, "systemctl daemon-reload") {
		t.Error("install must reload systemd after rewriting the unit, or the old one stays live")
	}
	if !strings.Contains(script, "systemctl restart mvm-daemon") {
		t.Error("install must restart the daemon")
	}
}

// The heredoc is quoted ('MVMUNIT') so nothing in the unit body is expanded by
// the shell that installs it — the same class of bug as an apostrophe closing a
// quoted script.
func TestInstallUsesQuotedHeredoc(t *testing.T) {
	script := InstallDaemonUnitScript("/home/lima")
	if !strings.Contains(script, "<<'MVMUNIT'") {
		t.Errorf("heredoc must be quoted so the unit body is not shell-expanded:\n%s", script)
	}
}
