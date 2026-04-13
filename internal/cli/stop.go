package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newStopCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a running microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(limaClient, store, args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "skip graceful shutdown, kill immediately")

	return cmd
}

func runStop(limaClient *lima.Client, store *state.Store, name string, force bool) error {
	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}
	if vm.Status != "running" && vm.Status != "paused" {
		return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
	}

	// Apple VZ path — kill the mvm-vz process directly
	if vm.Backend == "applevz" {
		fmt.Printf("Stopping microVM '%s'...\n", name)
		vzBackend := vm_pkg.NewAppleVZBackend(mvmDir)
		vzBackend.StopVM(vm.PID)
		now := time.Now()
		store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
		fmt.Println("  ✓ VM stopped")
		return nil
	}

	// Firecracker path — ensure Lima is running
	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	// Check if actually running
	if !firecracker.IsRunning(limaClient, vm.PID) {
		fmt.Printf("  microVM %q was already stopped (stale state)\n", name)
		now := time.Now()
		return store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
	}

	fmt.Printf("Stopping microVM '%s'...\n", name)

	// Clean up port forwarding
	firecracker.RemovePortForwarding(limaClient, vm)

	// Resume if paused (needed for graceful shutdown)
	if vm.Status == "paused" {
		firecracker.Resume(limaClient, vm)
	}

	hostKeyPath := filepath.Join(firecracker.KeyDir, "mvm.id_ed25519")
	if force {
		// Force kill
		limaClient.Shell(fmt.Sprintf("sudo kill -9 %d 2>/dev/null || true", vm.PID))
		// Clean up networking and socket
		limaClient.Shell(fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true",
			firecracker.SocketPath(name), vm.TAPDevice))
		fmt.Println("  ✓ Force killed")
	} else {
		if err := firecracker.StopViaAgent(limaClient, vm, hostKeyPath); err != nil {
			return err
		}
		fmt.Println("  ✓ VM stopped")
	}

	now := time.Now()
	return store.UpdateVM(name, func(v *state.VM) {
		v.Status = "stopped"
		v.StoppedAt = &now
	})
}
