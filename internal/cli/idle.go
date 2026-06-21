package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

const launchdLabel = "com.mvm.idle-check"

func newIdleCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "idle",
		Short: "Manage auto-idle (pause VMs after inactivity)",
	}

	cmd.AddCommand(
		newIdleEnableCmd(),
		newIdleDisableCmd(),
		newIdleCheckCmd(limaClient, store),
		newIdleStatusCmd(),
	)

	return cmd
}

func newIdleEnableCmd() *cobra.Command {
	var timeout string

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable auto-idle (installs launchd agent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return enableIdle(timeout)
		},
	}

	cmd.Flags().StringVar(&timeout, "timeout", "5m", "idle timeout before auto-pause")
	return cmd
}

func newIdleDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Disable auto-idle (removes launchd agent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return disableIdle()
		},
	}
}

func newIdleStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show auto-idle status",
		RunE: func(cmd *cobra.Command, args []string) error {
			plistPath := plistPath()
			if _, err := os.Stat(plistPath); os.IsNotExist(err) {
				fmt.Println("Auto-idle: disabled")
				return nil
			}
			fmt.Println("Auto-idle: enabled")
			fmt.Printf("  Plist: %s\n", plistPath)
			return nil
		},
	}
}

// newIdleCheckCmd is the hidden command executed by launchd every 30s.
func newIdleCheckCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:    "check",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIdleCheck(limaClient, store)
		},
	}
}

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func enableIdle(timeout string) error {
	// Validate timeout
	if _, err := time.ParseDuration(timeout); err != nil {
		return fmt.Errorf("invalid timeout %q: %w", timeout, err)
	}

	// Find mvm binary
	mvmBin, err := os.Executable()
	if err != nil {
		mvmBin = "mvm"
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>idle</string>
        <string>check</string>
    </array>
    <key>StartInterval</key>
    <integer>30</integer>
    <key>StandardOutPath</key>
    <string>%s/idle.log</string>
    <key>StandardErrorPath</key>
    <string>%s/idle.log</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>MVM_IDLE_TIMEOUT</key>
        <string>%s</string>
    </dict>
</dict>
</plist>`, launchdLabel, mvmBin, mvmDir, mvmDir, mvmDir, timeout)

	path := plistPath()
	os.MkdirAll(filepath.Dir(path), 0o755)

	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}

	// Load the agent
	exec.Command("launchctl", "unload", path).Run() // ignore error
	if err := exec.Command("launchctl", "load", path).Run(); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}

	fmt.Printf("  ✓ Auto-idle enabled (timeout: %s)\n", timeout)
	fmt.Printf("    Plist: %s\n", path)
	fmt.Printf("    Log: %s/idle.log\n", mvmDir)
	return nil
}

func disableIdle() error {
	path := plistPath()
	exec.Command("launchctl", "unload", path).Run()
	os.Remove(path)
	fmt.Println("  ✓ Auto-idle disabled")
	return nil
}

// runIdleCheck is called by launchd every 30s. Checks all running VMs
// for idle timeout and pauses them.
func runIdleCheck(limaClient *lima.Client, store *state.Store) error {
	timeout := 5 * time.Minute
	if t := os.Getenv("MVM_IDLE_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}

	// Guard: skip if Lima isn't running (Firecracker backend)
	backend := store.GetBackend()
	if backend == "firecracker" {
		exists, _ := limaClient.VMExists()
		if !exists {
			return nil
		}
		status, _ := limaClient.VMStatus()
		if status != "Running" {
			return nil
		}
	}

	// Phase 1: collect idle VMs under the lock (read-only). We must NOT pause
	// here: the daemon's PauseVM calls store.UpdateVM, which takes the same
	// flock we hold inside Transact — pausing while holding it deadlocks the
	// idle checker against the daemon.
	type idleVM struct {
		name, backend string
		idle          time.Duration
	}
	var idle []idleVM
	if err := store.Transact(func(st *state.State) error {
		now := time.Now()
		for _, vm := range st.VMs {
			if vm.Status != "running" {
				continue
			}
			lastActive := vm.CreatedAt
			if vm.LastActivity != nil {
				lastActive = *vm.LastActivity
			}
			if d := now.Sub(lastActive); d > timeout {
				idle = append(idle, idleVM{name: vm.Name, backend: vm.Backend, idle: d})
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Phase 2: pause each idle VM with the lock released.
	for _, iv := range idle {
		paused := false
		switch {
		case iv.backend == "applevz":
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			paused = vm_pkg.NewAppleVZBackend(mvmDir).HelperClient(iv.name).Pause(ctx) == nil
			cancel()
		default:
			if sc, scErr := requireDaemon(); scErr == nil {
				paused = sc.PauseVM(context.Background(), iv.name) == nil
			} else if vm, _ := store.GetVM(iv.name); vm != nil {
				paused = firecracker.Pause(limaClient, vm) == nil
			}
		}
		if paused {
			store.UpdateVM(iv.name, func(v *state.VM) { v.Status = "paused" })
			fmt.Printf("[idle-check] Paused %s (idle %s)\n", iv.name, iv.idle.Round(time.Second))
		}
	}
	return nil
}

// TouchActivity updates LastActivity for a VM. Call from exec/ssh.
func TouchActivity(store *state.Store, name string) {
	now := time.Now()
	store.UpdateVM(name, func(v *state.VM) {
		v.LastActivity = &now
	})
}

// AutoResumeIfPaused resumes a paused VM and returns true if it was paused.
func AutoResumeIfPaused(limaClient *lima.Client, store *state.Store, vm *state.VM) bool {
	if vm.Status != "paused" {
		return false
	}
	if vm.Backend == "applevz" {
		// Resume via the per-VM mvm-vz helper (in-memory, near-instant).
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := vm_pkg.NewAppleVZBackend(mvmDir).HelperClient(vm.Name).Resume(ctx); err != nil {
			return false
		}
		store.UpdateVM(vm.Name, func(v *state.VM) { v.Status = "running" })
		fmt.Printf("  Auto-resumed %s\n", vm.Name)
		return true
	}
	if err := firecracker.Resume(limaClient, vm); err != nil {
		return false
	}
	store.UpdateVM(vm.Name, func(v *state.VM) {
		v.Status = "running"
	})
	fmt.Printf("  Auto-resumed %s\n", vm.Name)
	return true
}
