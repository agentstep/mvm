package state

import "fmt"

// DaemonUnitPath is where the daemon's systemd unit lives inside the Lima VM.
const DaemonUnitPath = "/etc/systemd/system/mvm-daemon.service"

// DaemonBinaryPath is the daemon binary inside the Lima VM.
const DaemonBinaryPath = "/opt/mvm/mvm-daemon"

// DaemonUnit renders the systemd unit for the in-Lima daemon.
//
// home is the Lima user's home directory. It is passed to the daemon so the
// control socket can be handed to that account: the daemon runs as root under
// systemd, but Lima's SSH socket-forwarder runs unprivileged, so a root-owned
// socket is unreachable from the macOS CLI. See internal/server/socketperm.go.
func DaemonUnit(home string) string {
	return fmt.Sprintf(`[Unit]
Description=MVM Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s system start
Restart=always
RestartSec=2
Environment=HOME=%s
Environment=MVM_SOCKET_OWNER_HOME=%s

[Install]
WantedBy=multi-user.target
`, DaemonBinaryPath, home, home)
}

// InstallDaemonUnitScript returns a shell script that writes the unit, reloads
// systemd and restarts the daemon.
//
// This is rewritten on every deploy, deliberately. The unit encodes the command
// the daemon is launched with, and that command has changed before: an installed
// unit kept invoking `mvm serve` after the CLI renamed it to `mvm system start`,
// so the service crash-looped — restart counter in the teens — while the binary
// on disk was perfectly good. Nothing detected it, because a stale unit and a
// broken daemon look identical from outside.
//
// The daemon is stopped before the binary is replaced: overwriting a running
// executable fails with ETXTBSY, and the failure is easy to miss because the
// old daemon keeps serving happily afterwards.
func InstallDaemonUnitScript(home string) string {
	return fmt.Sprintf(`set -e
sudo systemctl stop mvm-daemon 2>/dev/null || true
cat <<'MVMUNIT' | sudo tee %s >/dev/null
%sMVMUNIT
sudo systemctl daemon-reload
sudo systemctl enable mvm-daemon.service >/dev/null 2>&1
sudo systemctl restart mvm-daemon.service
`, DaemonUnitPath, DaemonUnit(home))
}
