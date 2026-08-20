package container

import (
	"strings"
	"testing"
)

func TestInitArgsUsesProcSelfExe(t *testing.T) {
	path, args := InitCommand()
	// /proc/self/exe pins the running inode. Re-execing /opt/mvm-agent by path
	// would let a binary upgrade — or a respawn after the file changed on disk —
	// produce an inner init speaking a different protocol version than the outer
	// agent that spawned it.
	if path != "/proc/self/exe" {
		t.Errorf("InitCommand path = %q, want /proc/self/exe", path)
	}
	if len(args) == 0 || args[0] != InitFlag {
		t.Errorf("InitCommand args = %v, want first arg %q", args, InitFlag)
	}
}

func TestIsInitProcess(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"mvm-agent"}, false},
		{[]string{"mvm-agent", InitFlag}, true},
		{[]string{"mvm-agent", "--something-else"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsInitProcess(c.argv); got != c.want {
			t.Errorf("IsInitProcess(%v) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// TestCloneFlagsExcludeNetworkAndUser pins two deliberate exclusions.
//
// Network: sharing the guest netns is what keeps the guest IP, iptables DNAT
// port forwarding, the tcp_forward handler and in-guest network policy working
// unchanged — which matters when the success criterion for this whole change is
// parity. It costs nothing in isolation terms, since the control channel is
// vsock and carries no IP.
//
// User: the guest is root-only by design, CLONE_NEWUSER is the namespace most
// likely to be restricted, and it would complicate every uid-sensitive path
// (exec -u, volume ownership) for no gain.
func TestCloneFlagsExcludeNetworkAndUser(t *testing.T) {
	flags := CloneFlags()
	if flags&flagNewNet != 0 {
		t.Error("CLONE_NEWNET must not be set — the netns is deliberately shared")
	}
	if flags&flagNewUser != 0 {
		t.Error("CLONE_NEWUSER must not be set — see the design doc")
	}
}

func TestCloneFlagsIncludeTheFour(t *testing.T) {
	flags := CloneFlags()
	for _, tc := range []struct {
		name string
		bit  uintptr
	}{
		{"CLONE_NEWPID", flagNewPid},
		{"CLONE_NEWNS", flagNewNS},
		{"CLONE_NEWIPC", flagNewIPC},
		{"CLONE_NEWUTS", flagNewUTS},
	} {
		if flags&tc.bit == 0 {
			t.Errorf("%s must be set", tc.name)
		}
	}
}

// TestDescribe pins that the diagnostic string names exactly the namespaces
// that are actually created. Claiming a net or user namespace in a log line
// would be worse than saying nothing: it invites a reader to assume an
// isolation boundary that does not exist.
func TestDescribe(t *testing.T) {
	got := Describe()
	if got != "pid, mnt, ipc, uts" {
		t.Errorf("Describe() = %q, want exactly the four unshared namespaces", got)
	}
	for _, absent := range []string{"net", "user"} {
		if strings.Contains(got, absent) {
			t.Errorf("Describe() = %q must not name %q — it is shared, not unshared", got, absent)
		}
	}
}
