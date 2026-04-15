package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newStopCmd(store *state.Store) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a running microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(store, args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "skip graceful shutdown, kill immediately")

	return cmd
}

func runStop(store *state.Store, name string, force bool) error {
	// Check if this is an Apple VZ VM (local state).
	vm, _ := store.GetVM(name)
	if vm != nil && vm.Backend == "applevz" {
		if vm.Status != "running" && vm.Status != "paused" {
			return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
		}
		fmt.Printf("Stopping microVM '%s'...\n", name)
		vzBackend := vm_pkg.NewAppleVZBackend(mvmDir)
		if err := vzBackend.StopVM(name, vm.PID); err != nil {
			fmt.Printf("  Warning: %v\n", err)
		}
		now := time.Now()
		store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
		fmt.Println("  ✓ VM stopped")
		return nil
	}

	// Firecracker path — use daemon API
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	fmt.Printf("Stopping microVM '%s'...\n", name)
	ctx := context.Background()
	if err := sc.StopVM(ctx, name, force); err != nil {
		return err
	}

	if force {
		fmt.Println("  ✓ Force killed")
	} else {
		fmt.Println("  ✓ VM stopped")
	}
	return nil
}
