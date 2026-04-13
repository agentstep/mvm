package firecracker

import (
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

// === StartScript generation tests ===

func TestStartScriptContainsVMName(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("my-test-vm", alloc, 0, 0)

	if !strings.Contains(script, "my-test-vm") {
		t.Error("script should contain VM name")
	}
}

func TestStartScriptSetsUpTAPBeforeFirecracker(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	tapIdx := strings.Index(script, "ip tuntap add")
	fcIdx := strings.Index(script, "firecracker")
	if tapIdx < 0 || fcIdx < 0 {
		t.Fatal("should have both TAP setup and firecracker start")
	}
	if tapIdx > fcIdx {
		t.Error("TAP device must be set up BEFORE starting Firecracker")
	}
}

func TestStartScriptUsesSparseCopy(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	if !strings.Contains(script, "--sparse=always") {
		t.Error("should use sparse copy for rootfs (performance)")
	}
}

func TestStartScriptCreatesLogFile(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	if !strings.Contains(script, "firecracker.log") {
		t.Error("should create firecracker.log")
	}
}

func TestStartScriptDetachesFirecracker(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	// Firecracker must run in background (setsid + &)
	if !strings.Contains(script, "setsid") {
		t.Error("should use setsid to detach Firecracker")
	}
}

func TestStartScriptReportsPID(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	if !strings.Contains(script, "PID:$FC_PID") {
		t.Error("should output PID: prefix for parsing")
	}
}

func TestStartScriptChecksProcessAlive(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	// Script should verify FC process started successfully
	if !strings.Contains(script, "kill -0") {
		t.Error("should check process is alive after start")
	}
}

// === StartExistingScript tests ===

func TestStartExistingScriptPreservesRootfs(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartExistingScript("install-test", alloc, 2, 1024)

	// CRITICAL: must NOT copy base.ext4 (would destroy installed packages)
	if strings.Contains(script, "base.ext4") {
		t.Error("StartExistingScript must NOT copy base.ext4 — this destroys user's installed packages")
	}
}

func TestStartExistingScriptVerifiesRootfsExists(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartExistingScript("verify", alloc, 0, 0)

	if !strings.Contains(script, "if [ ! -f") {
		t.Error("should verify rootfs exists before boot")
	}
}

func TestStartExistingScriptUsesCustomResources(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartExistingScript("res-test", alloc, 4, 2048)

	// The config JSON embedded in the script should have custom values
	if !strings.Contains(script, `"vcpu_count": 4`) {
		t.Error("should use custom vcpu_count")
	}
	if !strings.Contains(script, `"mem_size_mib": 2048`) {
		t.Error("should use custom mem_size_mib")
	}
}

// === StartFromSnapshotScript tests ===

func TestStartFromSnapshotScriptCopiesSnapshotFiles(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartFromSnapshotScript("snap", alloc)

	if !strings.Contains(script, "mem_file") {
		t.Error("should copy memory file")
	}
	if !strings.Contains(script, "rootfs.ext4") {
		t.Error("should copy rootfs")
	}
}

func TestStartFromSnapshotScriptWaitsForSocket(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartFromSnapshotScript("snap", alloc)

	// Must wait for API socket before sending snapshot/load
	if !strings.Contains(script, "test -S") {
		t.Error("should wait for API socket to be ready")
	}
}

func TestStartFromSnapshotScriptUsesResumeVM(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartFromSnapshotScript("snap", alloc)

	if !strings.Contains(script, `"resume_vm": true`) {
		t.Error("should resume VM after loading snapshot")
	}
}

func TestSnapshotScriptNetworkOverridesWithCustomIndex(t *testing.T) {
	alloc := state.AllocateNet(5)
	script := StartFromSnapshotScript("snap-net", alloc)

	// CRITICAL: Must use network_overrides inline in /snapshot/load
	// FC v1.13 rejects PUT /network-interfaces after snapshot load
	if !strings.Contains(script, "network_overrides") {
		t.Error("MUST use network_overrides for TAP remapping during snapshot restore")
	}
	if !strings.Contains(script, "tap5") {
		t.Error("should use correct TAP device from allocation")
	}
}

func TestStartFromSnapshotScriptNoConfigFile(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartFromSnapshotScript("snap", alloc)

	// Snapshot restore starts FC without --config-file (uses API instead)
	if strings.Contains(script, "--config-file") {
		t.Error("snapshot restore should NOT use --config-file")
	}
}

// === Path helper tests ===

func TestSocketPathFormat(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"test", "/run/mvm/test.socket"},
		{"my-vm", "/run/mvm/my-vm.socket"},
		{"vm.1", "/run/mvm/vm.1.socket"},
	}
	for _, tt := range tests {
		got := SocketPath(tt.name)
		if got != tt.want {
			t.Errorf("SocketPath(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestVMDirFormat(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"test", "/opt/mvm/vms/test"},
		{"my-vm", "/opt/mvm/vms/my-vm"},
	}
	for _, tt := range tests {
		got := VMDir(tt.name)
		if got != tt.want {
			t.Errorf("VMDir(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// === Vsock socket cleanup (Bug: stale vsock causes "Address in use") ===

func TestStartScriptCleansVsockSocket(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	if !strings.Contains(script, ".vsock") {
		t.Error("StartScript should clean up stale vsock socket")
	}
}

func TestStartExistingScriptCleansVsockSocket(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartExistingScript("test", alloc, 0, 0)

	if !strings.Contains(script, ".vsock") {
		t.Error("StartExistingScript should clean up stale vsock socket")
	}
}

func TestStartFromSnapshotScriptCleansVsockSocket(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartFromSnapshotScript("test", alloc)

	if !strings.Contains(script, ".vsock") {
		t.Error("StartFromSnapshotScript should clean up stale vsock socket")
	}
}

// === Install constants ===

func TestFirecrackerConstants(t *testing.T) {
	if DefaultVersion == "" {
		t.Error("DefaultVersion should not be empty")
	}
	if Arch != "aarch64" {
		t.Errorf("Arch = %q, want aarch64 (Apple Silicon)", Arch)
	}
}
