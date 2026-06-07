package cli

import (
	"context"
	"fmt"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newSuspendCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <name>",
		Short: "Suspend a microVM to disk, freeing its RAM (auto-resumes on exec)",
		Long: `Suspend a running microVM. Unlike pause (which keeps the guest's
memory resident), suspend snapshots the VM to disk and stops the Firecracker
process, releasing all of its RAM back to the host. The next 'mvm exec' on the
VM transparently restores it from the snapshot.

Use suspend to keep many idle sandboxes around at near-zero cost:

  mvm suspend my-app       # snapshot + free RAM
  mvm exec my-app -- ls    # transparently restores, then runs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSuspend(store, args[0])
		},
	}
}

func runSuspend(store *state.Store, name string) error {
	vm, _ := store.GetVM(name)
	if vm != nil && vm.Backend == "applevz" {
		return fmt.Errorf("suspend is not supported on the Apple VZ backend. It requires Firecracker's snapshot support")
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	if err := sc.SuspendVM(context.Background(), name); err != nil {
		return err
	}

	fmt.Printf("  ✓ %s suspended — RAM freed (auto-resumes on: mvm exec %s)\n", name, name)
	return nil
}
