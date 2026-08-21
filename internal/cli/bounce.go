package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/agentstep/mvm/internal/state"
	"github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newBounceCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "bounce <name>",
		Short: "Restart user code inside a microVM without rebooting it",
		Long: `Restart everything running inside a sandbox, without rebooting the VM.

Faster and less disruptive than stop/start: the VM keeps running, so host-side
connections and published ports stay up, and there is no boot to wait through.

  Resets:   processes, PTYs, IPC objects, /dev/shm contents, hostname
  Persists: every file, routes, iptables rules, published ports

In-flight ` + "`mvm exec`" + ` sessions are terminated — they were running in the
namespace being replaced. Use ` + "`mvm stop`" + `/` + "`mvm start`" + ` if you need the kernel
or network state reset too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBounce(store, args[0])
		},
	}
}

func runBounce(store *state.Store, name string) error {
	// applevz has no daemon, so the CLI dials the guest agent directly.
	// Firecracker goes through the daemon like every other verb, which is also
	// what makes this work against a remote daemon over MVM_REMOTE.
	v, lookupErr := store.GetVM(name)
	if lookupErr == nil && v.Backend == "applevz" {
		if v.Status != "running" {
			return fmt.Errorf("VM %q is %s (bounce needs a running VM)", name, v.Status)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := vm.NewAppleVZBackend(mvmDir).AgentClient(name).Bounce(ctx); err != nil {
			return fmt.Errorf("bounce %q: %w", name, err)
		}
		fmt.Printf("  Bounced '%s' — processes restarted, files and network untouched\n", name)
		return nil
	}

	sc, err := requireDaemon()
	if err != nil {
		if lookupErr != nil {
			return lookupErr
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := sc.Bounce(ctx, name); err != nil {
		return fmt.Errorf("bounce %q: %w", name, err)
	}
	fmt.Printf("  Bounced '%s' — processes restarted, files and network untouched\n", name)
	return nil
}
