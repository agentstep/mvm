package cli

import (
	"fmt"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newPauseCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "pause <name>",
		Short: "Pause a running microVM (checkpoint in memory)",
		Long: `Pause a running microVM. The VM state is frozen in memory.
Resume instantly with 'mvm resume'. No CPU is consumed while paused.

  mvm pause my-app    # freeze VM
  mvm resume my-app   # instant resume`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPause(limaClient, store, args[0])
		},
	}
}

func newResumeCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <name>",
		Short: "Resume a paused microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResume(limaClient, store, args[0])
		},
	}
}

func runPause(limaClient *lima.Client, store *state.Store, name string) error {
	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}
	if vm.Backend == "applevz" {
		return fmt.Errorf("pause/resume is not supported on the Apple VZ backend. It requires Firecracker's snapshot support (M3+)")
	}
	if vm.Status != "running" {
		return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
	}

	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	if err := firecracker.Pause(limaClient, vm); err != nil {
		return err
	}

	if err := store.UpdateVM(name, func(v *state.VM) {
		v.Status = "paused"
	}); err != nil {
		return err
	}

	fmt.Printf("  ✓ %s paused (resume with: mvm resume %s)\n", name, name)
	return nil
}

func runResume(limaClient *lima.Client, store *state.Store, name string) error {
	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}
	if vm.Backend == "applevz" {
		return fmt.Errorf("pause/resume is not supported on the Apple VZ backend")
	}
	if vm.Status != "paused" {
		return fmt.Errorf("microVM %q is not paused (status: %s)", name, vm.Status)
	}

	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	if err := firecracker.Resume(limaClient, vm); err != nil {
		return err
	}

	if err := store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
	}); err != nil {
		return err
	}

	fmt.Printf("  ✓ %s resumed\n", name)
	return nil
}
