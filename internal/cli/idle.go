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
	var suspendAfter string

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable auto-idle (installs launchd agent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return enableIdle(timeout, suspendAfter)
		},
	}

	cmd.Flags().StringVar(&timeout, "timeout", "5m", "idle timeout before auto-pause (RAM stays resident)")
	cmd.Flags().StringVar(&suspendAfter, "suspend-after", "", "idle timeout before suspend-to-disk (frees RAM); empty disables")
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

func enableIdle(timeout, suspendAfter string) error {
	// Validate timeout
	if _, err := time.ParseDuration(timeout); err != nil {
		return fmt.Errorf("invalid timeout %q: %w", timeout, err)
	}
	if suspendAfter != "" {
		if _, err := time.ParseDuration(suspendAfter); err != nil {
			return fmt.Errorf("invalid --suspend-after %q: %w", suspendAfter, err)
		}
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
        <key>MVM_SUSPEND_TIMEOUT</key>
        <string>%s</string>
    </dict>
</dict>
</plist>`, launchdLabel, mvmBin, mvmDir, mvmDir, mvmDir, timeout, suspendAfter)

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
	// suspendTimeout is the deeper tier: snapshot to disk and free RAM.
	// Disabled unless MVM_SUSPEND_TIMEOUT is set.
	var suspendTimeout time.Duration
	if t := os.Getenv("MVM_SUSPEND_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			suspendTimeout = d
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

	// Check all idle-eligible VMs.
	return store.Transact(func(st *state.State) error {
		now := time.Now()
		for _, vm := range st.VMs {
			if vm.Backend == "applevz" {
				continue // no pause/snapshot support
			}
			if vm.Status != "running" && vm.Status != "paused" {
				continue
			}

			// Check last activity
			lastActive := vm.CreatedAt
			if vm.LastActivity != nil {
				lastActive = *vm.LastActivity
			}
			idle := now.Sub(lastActive)

			// Tier 2 (deepest): suspend-to-disk frees the guest's RAM. Applies
			// to both running and already-paused VMs once they're idle long
			// enough. Requires the daemon (snapshot machinery lives there).
			if suspendTimeout > 0 && idle > suspendTimeout {
				if sc, scErr := requireDaemon(); scErr == nil {
					if err := sc.SuspendVM(context.Background(), vm.Name); err == nil {
						vm.Status = "suspended"
						fmt.Printf("[idle-check] Suspended %s (idle %s)\n", vm.Name, idle.Round(time.Second))
						continue
					}
				}
			}

			// Tier 1: pause (freeze vCPUs, RAM resident) for running VMs.
			if vm.Status == "running" && idle > timeout {
				sc, scErr := requireDaemon()
				if scErr == nil {
					if err := sc.PauseVM(context.Background(), vm.Name); err == nil {
						vm.Status = "paused"
						fmt.Printf("[idle-check] Paused %s (idle %s)\n", vm.Name, idle.Round(time.Second))
					}
				} else if err := firecracker.Pause(limaClient, vm); err == nil {
					vm.Status = "paused"
					fmt.Printf("[idle-check] Paused %s (idle %s)\n", vm.Name, idle.Round(time.Second))
				}
			}
		}
		return nil
	})
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
		return false
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
