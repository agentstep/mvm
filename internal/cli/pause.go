package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newPauseCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "pause <name>",
		Short: "Pause a running microVM (checkpoint in memory)",
		Long: `Pause a running microVM. The VM state is frozen in memory.
Resume instantly with 'mvm resume'. No CPU is consumed while paused.

  mvm pause my-app    # freeze VM
  mvm resume my-app   # instant resume`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPause(store, args[0])
		},
	}
}

func newResumeCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <name>",
		Short: "Resume a paused microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResume(store, args[0])
		},
	}
}

func runPause(store *state.Store, name string) error {
	vm, _ := store.GetVM(name)

	// Apple VZ backend: pause via the per-VM mvm-vz helper. This is a
	// memory-resident pause (vCPUs frozen, RAM unchanged); resume is
	// essentially instant. No daemon is involved on this path.
	if vm != nil && vm.Backend == "applevz" {
		if vm.Status != "running" {
			return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := vm_pkg.NewAppleVZBackend(mvmDir).HelperClient(name).Pause(ctx); err != nil {
			return fmt.Errorf("pause %q: %w", name, err)
		}
		store.UpdateVM(name, func(v *state.VM) { v.Status = "paused" })
		fmt.Printf("  ✓ %s paused (resume with: mvm resume %s)\n", name, name)
		return nil
	}

	// Firecracker path — use daemon API
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := sc.PauseVM(ctx, name); err != nil {
		return err
	}

	fmt.Printf("  ✓ %s paused (resume with: mvm resume %s)\n", name, name)
	return nil
}

func runResume(store *state.Store, name string) error {
	vm, _ := store.GetVM(name)

	// Apple VZ backend: resume via the per-VM mvm-vz helper.
	if vm != nil && vm.Backend == "applevz" {
		if vm.Status != "paused" {
			return fmt.Errorf("microVM %q is not paused (status: %s)", name, vm.Status)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := vm_pkg.NewAppleVZBackend(mvmDir).HelperClient(name).Resume(ctx); err != nil {
			return fmt.Errorf("resume %q: %w", name, err)
		}
		store.UpdateVM(name, func(v *state.VM) { v.Status = "running" })
		fmt.Printf("  ✓ %s resumed\n", name)
		return nil
	}

	// Firecracker path — use daemon API
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := sc.ResumeVM(ctx, name); err != nil {
		return err
	}

	fmt.Printf("  ✓ %s resumed\n", name)
	return nil
}
