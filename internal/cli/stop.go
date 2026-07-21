package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newStopCmd(store *state.Store) *cobra.Command {
	var (
		signalName string
		timeout    int
		all        bool
	)

	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Gracefully stop a running microVM",
		Long: `Stop a running microVM. Sends --signal (default TERM) and waits up to
--time seconds before force-killing.

  mvm stop mybox
  mvm stop mybox -s KILL     # skip graceful shutdown, kill immediately
  mvm stop mybox -t 10       # wait up to 10s before killing
  mvm stop --all`,
		Args: func(cmd *cobra.Command, args []string) error {
			allFlag, _ := cmd.Flags().GetBool("all")
			if allFlag {
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("requires exactly 1 argument (or --all)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return runStopAll(store, signalName, timeout)
			}
			return runStop(store, args[0], signalName, timeout)
		},
	}

	cmd.Flags().StringVarP(&signalName, "signal", "s", "TERM", "signal to send (TERM graceful, KILL immediate)")
	cmd.Flags().IntVarP(&timeout, "time", "t", 5, "seconds to wait for graceful stop before killing")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "stop all running microVMs")

	return cmd
}

// signalIsKill reports whether a --signal value means "kill immediately".
func signalIsKill(name string) bool {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "KILL", "9":
		return true
	default:
		return false
	}
}

// runStop stops one VM. signalName selects graceful (TERM) vs immediate (KILL);
// timeoutSec is the graceful grace period (honored on applevz; advisory on the
// Firecracker daemon path, whose wire contract exposes only a force bool).
func runStop(store *state.Store, name, signalName string, timeoutSec int) error {
	force := signalIsKill(signalName)

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
		// Port forwarders (-p) are a detached process independent of the VM
		// helper — must be torn down explicitly or the host listener leaks
		// past the VM's lifetime.
		killForwarder(store, name, vm.ForwarderPID)
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
	if err := sc.StopVM(context.Background(), name, force); err != nil {
		return err
	}

	if force {
		fmt.Println("  ✓ Force killed")
	} else {
		fmt.Println("  ✓ VM stopped")
	}
	return nil
}

// runStopAll stops every running/paused microVM across both backends, using the
// same merged local-applevz + daemon view as `mvm ls` / delete --all.
func runStopAll(store *state.Store, signalName string, timeoutSec int) error {
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	var daemonVMs []server.VMResponse
	if sc, err := requireDaemon(); err == nil {
		if vms, err := sc.ListVMs(context.Background()); err == nil {
			daemonVMs = vms
		}
	}
	vms := mergeVMResponses(localVMs, daemonVMs)
	stopped := 0
	for _, vm := range vms {
		if vm.Status != "running" && vm.Status != "paused" {
			continue
		}
		if err := runStop(store, vm.Name, signalName, timeoutSec); err != nil {
			fmt.Printf("  Warning: failed to stop %s: %v\n", vm.Name, err)
			continue
		}
		stopped++
	}
	if stopped == 0 {
		fmt.Println("No running microVMs to stop.")
	}
	return nil
}
