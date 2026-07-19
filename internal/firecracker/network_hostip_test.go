package firecracker

import (
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

type recordingExecutor struct {
	commands []string
}

func (r *recordingExecutor) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	return "", nil
}
func (r *recordingExecutor) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	return r.Run(command)
}

func TestSetupPortForwardingOmitsDestFilterWhenHostIPEmpty(t *testing.T) {
	ex := &recordingExecutor{}
	vm := &state.VM{GuestIP: "172.16.0.2", Ports: []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}}}
	if err := SetupPortForwarding(ex, vm); err != nil {
		t.Fatalf("SetupPortForwarding: %v", err)
	}
	if len(ex.commands) != 1 {
		t.Fatalf("expected 1 command, got %d: %v", len(ex.commands), ex.commands)
	}
	if strings.Contains(ex.commands[0], " -d ") {
		t.Errorf("command should have no -d filter when HostIP is empty: %q", ex.commands[0])
	}
}

func TestSetupPortForwardingAddsDestFilterWhenHostIPSet(t *testing.T) {
	ex := &recordingExecutor{}
	vm := &state.VM{GuestIP: "172.16.0.2", Ports: []state.PortMap{{HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80, Proto: "tcp"}}}
	if err := SetupPortForwarding(ex, vm); err != nil {
		t.Fatalf("SetupPortForwarding: %v", err)
	}
	if !strings.Contains(ex.commands[0], "-d 127.0.0.1") {
		t.Errorf("command should filter on -d 127.0.0.1: %q", ex.commands[0])
	}
	// Both PREROUTING and OUTPUT rules get the filter.
	if strings.Count(ex.commands[0], "-d 127.0.0.1") != 2 {
		t.Errorf("expected -d filter on both PREROUTING and OUTPUT rules: %q", ex.commands[0])
	}
}

func TestRemovePortForwardingMatchesHostIPFilter(t *testing.T) {
	ex := &recordingExecutor{}
	vm := &state.VM{GuestIP: "172.16.0.2", Ports: []state.PortMap{{HostIP: "192.168.1.5", HostPort: 53, GuestPort: 53, Proto: "udp"}}}
	RemovePortForwarding(ex, vm)
	if len(ex.commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(ex.commands))
	}
	if !strings.Contains(ex.commands[0], "-d 192.168.1.5") {
		t.Errorf("delete command should match the same -d filter that was added: %q", ex.commands[0])
	}
}
