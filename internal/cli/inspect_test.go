package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestInspectResponseFromLocalVM(t *testing.T) {
	now := time.Now()
	vm := &state.VM{
		Name:      "web",
		Status:    "running",
		GuestIP:   "192.168.64.5",
		PID:       123,
		Backend:   "applevz",
		Ports:     []state.PortMap{{HostPort: 3000, GuestPort: 3000, Proto: "tcp"}},
		CreatedAt: now,
		Spec:      &state.VMSpec{Cpus: 4, NetPolicy: "deny"},
		// internal runtime fields that must NOT leak into inspect output:
		SocketPath: "/run/mvm/web.sock",
		TAPIP:      "172.16.0.1",
	}

	resp := inspectResponseFromLocalVM(vm)

	if resp.Name != "web" || resp.Status != "running" || resp.Backend != "applevz" {
		t.Errorf("resp = %+v, want identity fields copied", resp)
	}
	if resp.Spec == nil || resp.Spec.Cpus != 4 {
		t.Errorf("resp.Spec = %+v, want the VM's spec", resp.Spec)
	}

	// Schema check: the local path must emit the same shape as the daemon —
	// no state.VM internals like socket_path/tap_ip.
	data, _ := json.Marshal(resp)
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	for _, forbidden := range []string{"socket_path", "tap_ip", "tap_device", "guest_mac", "rootfs_path"} {
		if _, ok := m[forbidden]; ok {
			t.Errorf("inspect output leaks internal field %q", forbidden)
		}
	}
}
