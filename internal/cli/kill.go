package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newKillCmd(store *state.Store) *cobra.Command {
	var (
		signalName string
		all        bool
	)
	cmd := &cobra.Command{
		Use:   "kill <name>",
		Short: "Send a signal to a microVM immediately",
		Long: `Kill a microVM immediately — no graceful shutdown.

On applevz the signal is delivered to the VM helper process, so --signal is
honored. On Firecracker the daemon force-kills (SIGKILL) regardless of --signal.

  mvm kill mybox
  mvm kill mybox -s TERM
  mvm kill --all`,
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
				return runKillAll(store, signalName)
			}
			return runKill(store, args[0], signalName)
		},
	}
	cmd.Flags().StringVarP(&signalName, "signal", "s", "KILL", "signal to send (applevz only — FC always SIGKILLs)")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "kill all running microVMs")
	return cmd
}

// parseSignal maps a signal name (with/without SIG prefix, or a number) to a
// syscall.Signal. Unknown values default to SIGKILL — kill means "now".
func parseSignal(name string) syscall.Signal {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "KILL", "9":
		return syscall.SIGKILL
	case "TERM", "15":
		return syscall.SIGTERM
	case "INT", "2":
		return syscall.SIGINT
	case "HUP", "1":
		return syscall.SIGHUP
	case "QUIT", "3":
		return syscall.SIGQUIT
	default:
		return syscall.SIGKILL
	}
}

func runKill(store *state.Store, name, signalName string) error {
	if vm, _ := store.GetVM(name); vm != nil && vm.Backend == "applevz" {
		if vm.Status != "running" && vm.Status != "paused" {
			return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
		}
		sig := parseSignal(signalName)
		if vm.PID > 0 {
			if proc, err := os.FindProcess(vm.PID); err == nil {
				_ = proc.Signal(sig)
			}
		}
		killForwarder(store, name, vm.ForwarderPID)
		now := time.Now()
		store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
		fmt.Printf("  ✓ Killed %s\n", name)
		return nil
	}
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	if err := sc.StopVM(context.Background(), name, true); err != nil {
		return err
	}
	fmt.Printf("  ✓ Killed %s\n", name)
	return nil
}

func runKillAll(store *state.Store, signalName string) error {
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
	killed := 0
	for _, vm := range vms {
		if vm.Status != "running" && vm.Status != "paused" {
			continue
		}
		if err := runKill(store, vm.Name, signalName); err != nil {
			fmt.Printf("  Warning: failed to kill %s: %v\n", vm.Name, err)
			continue
		}
		killed++
	}
	if killed == 0 {
		fmt.Println("No running microVMs to kill.")
	}
	return nil
}
