package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
	"github.com/agentstep/mvm/internal/vzhelper"
)

// withTestMvmDir points the package-level mvmDir (normally set once in
// root.go's newRootCmd from $HOME) at a scratch directory for the duration
// of the test, restoring the previous value afterward. runDeleteAppleVZ
// resolves the VM state dir and IPC socket path relative to mvmDir, so
// tests need it pointed somewhere disposable rather than a real ~/.mvm.
func withTestMvmDir(t *testing.T) string {
	t.Helper()
	old := mvmDir
	dir := t.TempDir()
	mvmDir = dir
	t.Cleanup(func() { mvmDir = old })
	return dir
}

func TestRunDeleteAppleVZRunningWithoutForceRefuses(t *testing.T) {
	dir := withTestMvmDir(t)
	store := state.NewStore(filepath.Join(dir, "state.json"))

	vm := &state.VM{Name: "vz-running", Backend: "applevz", Status: "running", CreatedAt: time.Now()}
	if err := store.AddVM(vm); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	vmDir := filepath.Join(dir, "vms", "vz-running")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatalf("mkdir vm dir: %v", err)
	}

	if err := runDeleteAppleVZ(store, "vz-running", vm, false); err == nil {
		t.Fatal("runDeleteAppleVZ with running VM and force=false: want error, got nil")
	}

	// Nothing should have been touched: state entry and dir both survive.
	if _, err := store.GetVM("vz-running"); err != nil {
		t.Fatalf("GetVM after refused delete: %v (want VM to still exist)", err)
	}
	if _, err := os.Stat(vmDir); err != nil {
		t.Fatalf("vm dir after refused delete: %v (want it to still exist)", err)
	}
}

func TestRunDeleteAppleVZStoppedRemovesStateDirSocketAndEntry(t *testing.T) {
	dir := withTestMvmDir(t)
	store := state.NewStore(filepath.Join(dir, "state.json"))

	vm := &state.VM{Name: "vz-stopped", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}
	if err := store.AddVM(vm); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	// Simulate leftover on-disk state: rootfs clone / console log / snapshot.
	vmDir := filepath.Join(dir, "vms", "vz-stopped")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatalf("mkdir vm dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "console.log"), []byte("boot log"), 0o644); err != nil {
		t.Fatalf("write console.log: %v", err)
	}

	// Simulate a stale IPC socket left behind by a crashed/killed helper
	// (a plain file stands in for the real Unix socket here — os.Remove
	// doesn't care which it is).
	sockPath := vzhelper.SocketPath(dir, "vz-stopped")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(sockPath, []byte{}, 0o644); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}

	if err := runDeleteAppleVZ(store, "vz-stopped", vm, false); err != nil {
		t.Fatalf("runDeleteAppleVZ: %v", err)
	}

	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Fatalf("vm dir after delete: err=%v, want IsNotExist", err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("ipc socket after delete: err=%v, want IsNotExist", err)
	}
	if _, err := store.GetVM("vz-stopped"); err == nil {
		t.Fatal("GetVM after delete: want error (VM should be gone from state)")
	}
}

func TestRunDeleteAppleVZMissingStateDirAndSocketIsNotAnError(t *testing.T) {
	dir := withTestMvmDir(t)
	store := state.NewStore(filepath.Join(dir, "state.json"))

	// No vmDir, no socket file created at all — delete must still succeed
	// (e.g. a VM whose start failed before writing anything to disk).
	vm := &state.VM{Name: "vz-bare", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}
	if err := store.AddVM(vm); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	if err := runDeleteAppleVZ(store, "vz-bare", vm, false); err != nil {
		t.Fatalf("runDeleteAppleVZ on bare VM: %v", err)
	}
	if _, err := store.GetVM("vz-bare"); err == nil {
		t.Fatal("GetVM after delete: want error")
	}
}
