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
	v, err := store.GetVM(name)
	if err != nil {
		return err
	}
	if v.Status != "running" {
		return fmt.Errorf("VM %q is %s (bounce needs a running VM)", name, v.Status)
	}

	// applevz has no daemon, so the CLI talks to the guest agent directly. The
	// Firecracker path goes through the daemon like every other verb.
	if v.Backend != "applevz" {
		return fmt.Errorf("mvm bounce currently supports the applevz backend only")
	}

	vzBackend := vm.NewAppleVZBackend(mvmDir)
	agent := vzBackend.AgentClient(name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := agent.Bounce(ctx); err != nil {
		return fmt.Errorf("bounce %q: %w", name, err)
	}
	fmt.Printf("  Bounced '%s' — processes restarted, files and network untouched\n", name)
	return nil
}
