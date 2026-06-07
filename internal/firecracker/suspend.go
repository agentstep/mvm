package firecracker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/state"
)

// SuspendDir returns the directory holding a VM's suspend-to-disk snapshot.
// Suspend snapshots are kept separate from user-created snapshots (under
// DataDir/snapshots) so they don't appear in `mvm snapshot list` and can be
// garbage-collected when the VM is deleted.
func SuspendDir(name string) string {
	return filepath.Join(DataDir(), "suspend", name)
}

// SuspendVM implements the "idle-to-zero" tier. It snapshots a running VM into
// its suspend dir and then terminates the Firecracker process (and any UFFD
// sidecar), releasing the VM's guest RAM back to the host. The VM keeps its
// state entry and network allocation so ResumeFromSuspend can restore it in
// place.
//
// Contrast with Pause, which only freezes vCPUs but keeps the full guest
// memory resident. Suspend frees memory at the cost of a snapshot now and a
// UFFD lazy-restore on the next exec — the building block for running many
// idle sandboxes on one host (cf. Sprites/Blaxel).
//
// Returns the suspend snapshot directory.
func SuspendVM(exec Executor, vm *state.VM) (string, error) {
	snapDir := SuspendDir(vm.Name)

	// Clear any stale suspend snapshot from a previous cycle so a partial
	// failure can't leave us restoring from mismatched files.
	if _, err := exec.Run(fmt.Sprintf("sudo rm -rf %s", snapDir)); err != nil {
		return "", fmt.Errorf("clear stale suspend snapshot: %w", err)
	}

	// SnapshotVM pauses the VM, writes snapshot.bin/mem.bin/rootfs.ext4 into
	// snapDir, and resumes. We terminate immediately afterward to free RAM.
	if err := SnapshotVM(exec, vm, snapDir); err != nil {
		return "", fmt.Errorf("snapshot for suspend: %w", err)
	}

	terminateVMProcess(exec, vm)
	return snapDir, nil
}

// ResumeFromSuspend restores a suspended VM from its suspend snapshot, reusing
// the VM's existing network allocation so the guest keeps the same IP/TAP and
// the vsock UDS path embedded in the snapshot stays valid. Returns the new
// (pid, socketPath, uffdPid).
func ResumeFromSuspend(exec Executor, vm *state.VM) (int, string, int, error) {
	snapDir := SuspendDir(vm.Name)
	if _, err := os.Stat(filepath.Join(snapDir, "meta.json")); err != nil {
		return 0, "", 0, fmt.Errorf("no suspend snapshot for %q (looked in %s)", vm.Name, snapDir)
	}
	alloc := state.AllocateNet(vm.NetIndex)
	return RestoreVMSnapshot(exec, vm.Name, snapDir, alloc)
}

// terminateVMProcess force-kills a VM's Firecracker process and UFFD sidecar,
// then removes its API socket and TAP device. Used by Suspend to free
// resources without deleting the VM's state entry, rootfs, or snapshot.
func terminateVMProcess(exec Executor, vm *state.VM) {
	if vm.UFFDPid > 0 {
		_ = KillUFFDHandler(vm.UFFDPid)
	}
	exec.Run(fmt.Sprintf("sudo kill -9 %d 2>/dev/null || true", vm.PID))
	exec.Run(fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true",
		SocketPath(vm.Name), vm.TAPDevice))
}
