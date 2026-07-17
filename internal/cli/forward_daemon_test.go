package cli

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestPortForwardProtoSupported(t *testing.T) {
	tests := []struct {
		proto string
		want  bool
	}{
		{"", true},    // parsePorts defaults empty to tcp
		{"tcp", true}, // explicit tcp
		{"udp", false},
		{"UDP", false}, // case-sensitive on purpose — parsePorts never uppercases
	}
	for _, tt := range tests {
		if got := portForwardProtoSupported(tt.proto); got != tt.want {
			t.Errorf("portForwardProtoSupported(%q) = %v, want %v", tt.proto, got, tt.want)
		}
	}
}

// TestForwardDaemonStatusJSONRoundTrip locks in the wire shape spawnPortForwarders
// depends on to synchronize with the detached __forward-ports child.
func TestForwardDaemonStatusJSONRoundTrip(t *testing.T) {
	want := forwardDaemonStatus{
		PID: 4242,
		Ports: []forwardDaemonPortResult{
			{HostPort: 8080, GuestPort: 80, Bound: true},
			{HostPort: 53, GuestPort: 53, Bound: false, Error: "proto \"udp\" not supported for applevz port forwarding (tcp only)"},
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got forwardDaemonStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.PID != want.PID || len(got.Ports) != len(want.Ports) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, want)
	}
	for i := range want.Ports {
		if got.Ports[i] != want.Ports[i] {
			t.Errorf("Ports[%d] = %+v, want %+v", i, got.Ports[i], want.Ports[i])
		}
	}
}

// TestKillForwarderNilPID confirms killForwarder is a safe no-op when there's
// no forwarder to kill — the common case for VMs started without -p, and
// must not touch the store (there may be no VM entry at all yet).
func TestKillForwarderNilPID(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	// No VM named "ghost" exists in this store at all. If killForwarder
	// tried to update it, this would surface as a test failure via a
	// (currently ignored, but let's make sure) error — the real assertion
	// is just "doesn't panic, doesn't hang".
	killForwarder(store, "ghost", 0)
	killForwarder(store, "ghost", -1)
}

// TestKillForwarderTerminatesRealProcess spawns a real child process,
// records its PID as a VM's ForwarderPID, and confirms killForwarder
// actually terminates it and clears the field — this is the exact lifecycle
// `mvm stop` depends on to avoid leaking a host-side listener process past
// the VM's own lifetime (Bug 2's "no orphaned listeners" requirement).
func TestKillForwarderTerminatesRealProcess(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.AddVM(&state.VM{Name: "vz-fwd", Backend: "applevz", Status: "running"}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dummy process: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap it as soon as it exits — without this, a killed child sits as a
	// zombie until Wait()'d, and kill(pid, 0) keeps reporting a zombie as
	// "alive" (it still holds a process-table slot), which would make both
	// killForwarder's own poll loop and this test's spin forever on a false
	// positive. In the real `mvm stop` flow this isn't an issue: the
	// forwarder was spawned by a since-exited `mvm start` process, so
	// by the time `mvm stop` runs it's already been reparented to (and gets
	// reaped by) launchd.
	waitDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waitDone) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waitDone
	})

	if err := store.UpdateVM("vz-fwd", func(v *state.VM) { v.ForwarderPID = pid }); err != nil {
		t.Fatalf("UpdateVM: %v", err)
	}

	killForwarder(store, "vz-fwd", pid)

	// Process must actually be gone.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.Process.Signal(syscall.Signal(0)) != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cmd.Process.Signal(syscall.Signal(0)) == nil {
		t.Fatal("killForwarder did not terminate the process")
	}

	vm, err := store.GetVM("vz-fwd")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if vm.ForwarderPID != 0 {
		t.Errorf("ForwarderPID = %d after killForwarder, want 0", vm.ForwarderPID)
	}
}
