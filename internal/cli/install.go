package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newInstallCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <name> -- <command>",
		Short: "Install packages at native speed (10x faster than exec)",
		Long: `Run a command against a VM's rootfs at native speed via chroot.

This bypasses the double-virtualization penalty by running the command
in Lima's chroot instead of inside the nested Firecracker VM. ~10x faster
for CPU-heavy operations like npm install, pip install, apt-get install.

The VM is briefly stopped while the command runs, then restarted.

  mvm install my-app -- npm install -g typescript
  mvm install my-app -- apt-get install -y postgresql
  mvm install my-app -- pip install flask`,
		// DisableFlagParsing so flags after -- aren't eaten by cobra
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("usage: mvm install <name> -- <command>")
			}
			name := args[0]
			// Find command after -- or after name
			var installArgs []string
			for i, arg := range args {
				if arg == "--" && i+1 < len(args) {
					installArgs = args[i+1:]
					break
				}
			}
			if len(installArgs) == 0 {
				installArgs = args[1:]
			}
			if len(installArgs) == 0 {
				return fmt.Errorf("no command specified")
			}
			return runInstall(limaClient, store, name, strings.Join(installArgs, " "))
		},
	}
	return cmd
}

func runInstall(limaClient *lima.Client, store *state.Store, name, command string) error {
	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}

	if vm.Backend == "applevz" {
		return fmt.Errorf("mvm install only works with the Firecracker backend (Apple VZ has no Lima)")
	}

	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	start := time.Now()
	fmt.Printf("Installing in '%s' at native speed...\n", name)

	// Step 1: Stop the VM if running (must fully stop — pause is not safe for rootfs mount)
	wasRunning := vm.Status == "running"
	wasPaused := vm.Status == "paused"

	if wasRunning || wasPaused {
		if wasPaused {
			fmt.Println("  Resuming paused VM...")
			if err := firecracker.Resume(limaClient, vm); err != nil {
				return fmt.Errorf("resume before stop: %w", err)
			}
		}

		// Sync guest filesystem via agent
		fmt.Println("  Syncing guest filesystem...")
		firecracker.AgentExec(limaClient, vm.GuestIP, "sync")

		// Full stop
		fmt.Println("  Stopping VM...")
		keyPath := filepath.Join(firecracker.KeyDir, "mvm.id_ed25519")
		if err := firecracker.StopViaAgent(limaClient, vm, keyPath); err != nil {
			return fmt.Errorf("stop failed (cannot safely mount rootfs): %w", err)
		}

		store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
		})
	} else if vm.Status != "stopped" {
		return fmt.Errorf("VM %q is in unexpected status %q", name, vm.Status)
	}

	// Step 2: Run command in chroot at native speed
	rootfsPath := vm.RootfsPath
	if rootfsPath == "" {
		rootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"
	}

	fmt.Printf("  Running: %s\n", command)
	chrootErr := firecracker.ChrootExec(limaClient, rootfsPath, command)
	if chrootErr != nil {
		fmt.Printf("  Warning: install command failed: %v\n", chrootErr)
		fmt.Println("  (VM will still be restarted)")
	}

	// Step 3: Always restart VM if it was running before (even on chroot failure)
	// Leaving a VM stopped after a failed npm install is worse than restarting it.
	// IMPORTANT: Use RestartExisting, not Start — Start() would overwrite the rootfs
	// with base.ext4, destroying everything we just installed.
	if wasRunning || wasPaused {
		fmt.Println("  Restarting VM...")
		alloc := state.AllocateNet(vm.NetIndex)

		pid, err := firecracker.StartExisting(limaClient, name, alloc, vm.Cpus, vm.MemoryMB)
		if err != nil {
			return fmt.Errorf("restart failed: %w (run 'mvm start %s' manually)", err, name)
		}

		store.UpdateVM(name, func(v *state.VM) {
			v.Status = "running"
			v.PID = pid
			v.SocketPath = firecracker.SocketPath(name)
		})

		if firecracker.WaitForGuest(limaClient, vm.GuestIP, 60*time.Second) {
			firecracker.SetupGuestNetworkViaAgent(limaClient, vm.GuestIP, vm.TAPIP)
		}
	}

	elapsed := time.Since(start).Round(time.Second)
	if chrootErr != nil {
		fmt.Printf("\n  ⚠ Completed with errors in %s\n", elapsed)
		return chrootErr
	}
	fmt.Printf("\n  ✓ Done in %s (native speed)\n", elapsed)
	return nil
}
