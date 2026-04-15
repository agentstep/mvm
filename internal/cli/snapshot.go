package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newSnapshotCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage VM snapshots",
		Aliases: []string{"snap"},
	}

	cmd.AddCommand(
		newSnapshotCreateCmd(limaClient, store),
		newSnapshotRestoreCmd(limaClient, store),
		newSnapshotListCmd(),
		newSnapshotDeleteCmd(),
	)

	return cmd
}

func newSnapshotCreateCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "create <vm-name> [snapshot-name]",
		Short: "Create a delta snapshot of a running VM",
		Long: `Create a snapshot capturing the current state of a running VM.

Set MVM_SNAPSHOT_KEY to encrypt snapshots with AES-256-GCM:
  export MVM_SNAPSHOT_KEY=mysecretkey
  mvm snapshot create my-app`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			vmName := args[0]
			snapName := vmName + "-snap"
			if len(args) > 1 {
				snapName = args[1]
			}
			return runSnapshotCreate(limaClient, store, vmName, snapName)
		},
	}
}

func newSnapshotRestoreCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <vm-name> <snapshot-name>",
		Short: "Restore a VM from a snapshot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSnapshotRestore(limaClient, store, args[0], args[1])
		},
	}
}

func newSnapshotListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all snapshots",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSnapshotList()
		},
	}
}

func newSnapshotDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <snapshot-name>",
		Short: "Delete a snapshot",
		Aliases: []string{"rm"},
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSnapshotDelete(args[0])
		},
	}
}

func snapshotBaseDir() string {
	return filepath.Join(mvmDir, "snapshots")
}

func runSnapshotCreate(limaClient *lima.Client, store *state.Store, vmName, snapName string) error {
	vm, err := store.GetVM(vmName)
	if err != nil {
		return err
	}
	if vm.Status != "running" {
		return fmt.Errorf("VM %q must be running to create a snapshot (status: %s)", vmName, vm.Status)
	}
	if vm.Backend == "applevz" {
		return fmt.Errorf("snapshots are not supported on the Apple VZ backend")
	}

	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	snapDir := filepath.Join(snapshotBaseDir(), snapName)
	fmt.Printf("Creating snapshot '%s' of VM '%s'...\n", snapName, vmName)

	if err := firecracker.SnapshotVM(limaClient, vm, snapDir); err != nil {
		return err
	}

	// Show size
	var totalSize int64
	filepath.Walk(snapDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})

	fmt.Printf("  ✓ Snapshot '%s' created (%.1f MB)\n", snapName, float64(totalSize)/(1024*1024))
	return nil
}

func runSnapshotRestore(limaClient *lima.Client, store *state.Store, vmName, snapName string) error {
	vm, err := store.GetVM(vmName)
	if err != nil {
		return err
	}
	if vm.Backend == "applevz" {
		return fmt.Errorf("snapshots are not supported on the Apple VZ backend")
	}

	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	snapDir := filepath.Join(snapshotBaseDir(), snapName)
	if _, err := os.Stat(snapDir); os.IsNotExist(err) {
		return fmt.Errorf("snapshot %q not found", snapName)
	}

	alloc := state.AllocateNet(vm.NetIndex)

	fmt.Printf("Restoring VM '%s' from snapshot '%s'...\n", vmName, snapName)
	pid, socketPath, err := firecracker.RestoreVMSnapshot(limaClient, vmName, snapDir, alloc)
	if err != nil {
		return err
	}

	// Update VM state with new PID and socket
	if err := store.UpdateVM(vmName, func(v *state.VM) {
		v.PID = pid
		v.SocketPath = socketPath
		v.Status = "running"
	}); err != nil {
		return fmt.Errorf("save VM state: %w", err)
	}

	fmt.Printf("  ✓ VM '%s' restored from '%s' (PID: %d)\n", vmName, snapName, pid)
	return nil
}

func runSnapshotList() error {
	snapshots, err := firecracker.ListSnapshots(snapshotBaseDir())
	if err != nil {
		return err
	}
	if len(snapshots) == 0 {
		fmt.Println("No snapshots.")
		return nil
	}

	for _, name := range snapshots {
		dir := filepath.Join(snapshotBaseDir(), name)
		var size int64
		encrypted := false
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				size += info.Size()
				if filepath.Ext(path) == ".enc" {
					encrypted = true
				}
			}
			return nil
		})
		enc := ""
		if encrypted {
			enc = " [encrypted]"
		}
		fmt.Printf("  %s  (%.1f MB)%s\n", name, float64(size)/(1024*1024), enc)
	}
	return nil
}

func runSnapshotDelete(snapName string) error {
	dir := filepath.Join(snapshotBaseDir(), snapName)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("snapshot %q not found", snapName)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Printf("  ✓ Snapshot '%s' deleted\n", snapName)
	return nil
}
