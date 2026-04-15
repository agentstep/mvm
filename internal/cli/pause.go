package cli

import (
	"context"
	"fmt"

	"github.com/agentstep/mvm/internal/state"
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
	// Check for Apple VZ backend (not supported)
	vm, _ := store.GetVM(name)
	if vm != nil && vm.Backend == "applevz" {
		return fmt.Errorf("pause/resume is not supported on the Apple VZ backend. It requires Firecracker's snapshot support (M3+)")
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
	// Check for Apple VZ backend (not supported)
	vm, _ := store.GetVM(name)
	if vm != nil && vm.Backend == "applevz" {
		return fmt.Errorf("pause/resume is not supported on the Apple VZ backend")
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
