package firecracker

import (
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// recordExecutor records every command it is asked to run so tests can assert
// on the side effects of host-shell-driven helpers.
type recordExecutor struct{ cmds []string }

func (r *recordExecutor) Run(command string) (string, error) {
	r.cmds = append(r.cmds, command)
	return "", nil
}

func (r *recordExecutor) RunWithTimeout(command string, _ time.Duration) (string, error) {
	r.cmds = append(r.cmds, command)
	return "", nil
}

func (r *recordExecutor) ranContaining(substr string) bool {
	for _, c := range r.cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestSuspendDir(t *testing.T) {
	t.Setenv("MVM_DATA_DIR", "/tmp/mvm-test")
	got := SuspendDir("my-app")
	want := "/tmp/mvm-test/suspend/my-app"
	if got != want {
		t.Fatalf("SuspendDir = %q, want %q", got, want)
	}
}

func TestTerminateVMProcess(t *testing.T) {
	t.Setenv("MVM_RUN_DIR", "/run/mvm-test")
	ex := &recordExecutor{}
	vm := &state.VM{Name: "t", PID: 4242, TAPDevice: "mvtap5"}

	terminateVMProcess(ex, vm)

	if !ex.ranContaining("kill -9 4242") {
		t.Errorf("expected the Firecracker PID to be killed; got cmds: %v", ex.cmds)
	}
	if !ex.ranContaining("ip link del mvtap5") {
		t.Errorf("expected the TAP device to be removed; got cmds: %v", ex.cmds)
	}
	if !ex.ranContaining(SocketPath("t")) {
		t.Errorf("expected the API socket %q to be removed; got cmds: %v", SocketPath("t"), ex.cmds)
	}
}

func TestResumeFromSuspendMissingSnapshot(t *testing.T) {
	t.Setenv("MVM_DATA_DIR", t.TempDir())
	ex := &recordExecutor{}
	vm := &state.VM{Name: "ghost", NetIndex: 0}

	_, _, _, err := ResumeFromSuspend(ex, vm)
	if err == nil {
		t.Fatal("expected an error resuming a VM with no suspend snapshot, got nil")
	}
	if !strings.Contains(err.Error(), "no suspend snapshot") {
		t.Errorf("unexpected error: %v", err)
	}
}
