package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/agentstep/mvm/internal/vzhelper"
	"github.com/spf13/cobra"
)

func newDeleteCmd(store *state.Store) *cobra.Command {
	var (
		force bool
		all   bool
	)

	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a microVM and all its resources",
		Aliases: []string{"rm"},
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
				return runDeleteAll(store, force)
			}
			return runDelete(store, args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "stop the VM first if running")
	cmd.Flags().BoolVar(&all, "all", false, "delete all microVMs")

	return cmd
}

// runDelete deletes a single microVM. Apple VZ VMs live purely in local
// state (~/.mvm/state.json + ~/.mvm/vms/<name>/) — the daemon has never
// heard of them (same reason exec/start/stop/pause/list special-case
// applevz instead of going through requireDaemon()). Checking local state
// first, before requiring the daemon, is what list.go's fix did for `mvm
// ls`; delete needs the identical special-case or it always reports
// microVM %q not found for a real, running applevz VM.
func runDelete(store *state.Store, name string, force bool) error {
	if vm, _ := store.GetVM(name); vm != nil && vm.Backend == "applevz" {
		return runDeleteAppleVZ(store, name, vm, force)
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Check VM status via listing
	vms, err := sc.ListVMs(ctx)
	if err != nil {
		return err
	}

	var found bool
	var status string
	for _, vm := range vms {
		if vm.Name == name {
			found = true
			status = vm.Status
			break
		}
	}
	if !found {
		return fmt.Errorf("microVM %q not found", name)
	}

	if status == "running" {
		if !force {
			return fmt.Errorf("microVM %q is running. Stop it first (mvm stop %s) or use --force", name, name)
		}
		fmt.Printf("Stopping microVM '%s'...\n", name)
		if err := sc.StopVM(ctx, name, true); err != nil {
			return err
		}
	}

	fmt.Printf("Deleting microVM '%s'...\n", name)
	if err := sc.DeleteVM(ctx, name); err != nil {
		return err
	}
	fmt.Println("  ✓ Deleted")

	return nil
}

// runDeleteAppleVZ tears down an applevz VM end-to-end: stop it (if running
// or paused, matching the same running/paused check and --force convention
// stop.go/delete's daemon path already use), kill any leftover port-forwarder
// process, remove the on-disk state dir (rootfs clone, console log, saved
// snapshot, machine-id), remove a stale IPC socket if the helper didn't clean
// up after itself, and finally drop the state.json entry.
func runDeleteAppleVZ(store *state.Store, name string, vm *state.VM, force bool) error {
	if vm.Status == "running" || vm.Status == "paused" {
		if !force {
			return fmt.Errorf("microVM %q is running. Stop it first (mvm stop %s) or use --force", name, name)
		}
		fmt.Printf("Stopping microVM '%s'...\n", name)
		vzBackend := vm_pkg.NewAppleVZBackend(mvmDir)
		if err := vzBackend.StopVM(name, vm.PID); err != nil {
			fmt.Printf("  Warning: %v\n", err)
		}
	}

	// Tear down any leftover port-forwarder (-p) process. Not gated on the
	// running/paused check above: a prior crash can leave ForwarderPID set
	// on a VM that state already calls "stopped" (see the defensive kill in
	// runStartAppleVZ), and delete should never leave an orphaned listener
	// behind regardless of the VM's last recorded status.
	killForwarder(store, name, vm.ForwarderPID)

	fmt.Printf("Deleting microVM '%s'...\n", name)

	vmDir := filepath.Join(mvmDir, "vms", name)
	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("remove vm state dir: %w", err)
	}

	// The mvm-vz helper unlinks its own IPC socket on a graceful stop, but a
	// killed/crashed helper (or one this process just SIGKILL'd via StopVM's
	// fallback path) can leave the socket file behind — clean it up
	// defensively so no stale ~/.mvm/run/vz-<name>.sock lingers.
	sockPath := vzhelper.SocketPath(mvmDir, name)
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("  Warning: remove ipc socket: %v\n", err)
	}

	if err := store.RemoveVM(name); err != nil {
		return err
	}
	fmt.Println("  ✓ Deleted")
	return nil
}

func runDeleteAll(store *state.Store, force bool) error {
	// Merge local applevz VMs with daemon (Firecracker) VMs, same shape as
	// `mvm ls` — see list.go's localApplevzVMs/mergeVMResponses.
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

	if len(vms) == 0 {
		fmt.Println("No microVMs to delete.")
		return nil
	}

	// Check for running VMs
	if !force {
		for _, vm := range vms {
			if vm.Status == "running" {
				return fmt.Errorf("some microVMs are still running. Stop them first or use --force")
			}
		}
	}

	for _, vm := range vms {
		if err := runDelete(store, vm.Name, force); err != nil {
			fmt.Printf("  Warning: failed to delete %s: %v\n", vm.Name, err)
		}
	}
	return nil
}
