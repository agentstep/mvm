package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

// waitForReady polls probe until it returns nil or timeout elapses,
// backing off between attempts (200ms, doubling, capped at 2s). Bridges the
// gap between a VM reporting "running" and its guest agent actually being
// reachable — neither backend's create path blocks on that today (Firecracker
// readiness happens in a daemon-side goroutine; see
// internal/server/routes.go's handleCreateVM).
func waitForReady(timeout time.Duration, probe func() error) error {
	deadline := time.Now().Add(timeout)
	backoff := 200 * time.Millisecond
	var lastErr error
	for {
		if err := probe(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for VM to become ready: %w", lastErr)
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// resolveImage maps the "base" sentinel to the implicit default rootfs
// (image="", exactly what runStart/handleCreateVM already treat as "use
// base.ext4"). "base" is not yet a real catalogued image — that arrives
// with the OCI image store (design spec step 3) — so this mapping lives
// entirely at the CLI layer.
func resolveImage(image string) string {
	if image == "base" {
		return ""
	}
	return image
}

// resolveRunName decides the VM's name and whether it should be deleted
// after its foreground command exits. An explicit --name opts into
// durability (never auto-deleted); with no --name, a fresh name is
// generated and the VM is ephemeral.
func resolveRunName(nameFlag string, existing map[string]bool) (name string, ephemeral bool) {
	if nameFlag != "" {
		return nameFlag, false
	}
	return GenerateVMName(existing), true
}

// existingVMNames returns every VM name currently known, merging local
// applevz state (the daemon never sees these — see list.go's
// localApplevzVMs) with the daemon's own list of Firecracker VMs.
// Best-effort on the daemon side: an applevz-only host with no daemon
// running is not an error here, matching runDeleteAll's daemon-merge
// pattern in delete.go.
func existingVMNames(store *state.Store) (map[string]bool, error) {
	names := map[string]bool{}

	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return nil, err
	}
	for _, vm := range localVMs {
		names[vm.Name] = true
	}

	if sc, err := requireDaemon(); err == nil {
		if vms, err := sc.ListVMs(context.Background()); err == nil {
			for _, vm := range vms {
				names[vm.Name] = true
			}
		}
	}

	return names, nil
}

func newRunCmd(store *state.Store) *cobra.Command {
	var (
		name        string
		detach      bool
		cpus        int
		memoryMB    int
		netPolicy   string
		ports       []string
		volumes     []string
		interactive bool
		tty         bool
		envVars     []string
		envFile     string
		user        string
		workdir     string
	)

	cmd := &cobra.Command{
		Use:   "run <image> [-- command [args...]]",
		Short: "Boot a VM from an image and run a command, ephemeral by default",
		Long: `Boot a VM from an image, image-first (Docker-style).

Without --name, the VM is auto-named and deleted after the command exits
(like docker run --rm). With --name, the VM persists. "base" is the
default rootfs — there is no other catalogued image yet.

  mvm run base -- ls /                  # ephemeral: boots, runs, deletes
  mvm run base --name mybox -- bash     # persists as "mybox"
  mvm run base -d                       # boot and detach, no command
  mvm run base -p 8080:80 -- serve`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			image := args[0]
			cmdArgs := args[1:]
			portMaps, err := parsePorts(ports)
			if err != nil {
				return err
			}
			volumes, err = parseVolumes(volumes)
			if err != nil {
				return err
			}
			allEnv, err := mergeEnvFile(envFile, envVars)
			if err != nil {
				return err
			}
			return runRun(store, image, cmdArgs, name, detach, cpus, memoryMB, netPolicy, portMaps, volumes, interactive || tty, workdir, allEnv, user)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "use this name and keep the VM after the command exits")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "boot and return immediately; do not run a foreground command")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVar(&memoryMB, "memory", 0, "RAM in MiB (default: 1024)")
	cmd.Flags().StringVar(&netPolicy, "net-policy", "open", "network policy: open, deny, or allow:domain1,domain2")
	cmd.Flags().StringArrayVarP(&ports, "publish", "p", nil, "publish port (hostPort:guestPort[/proto])")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "V", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open (foreground command only)")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY (foreground command only)")
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil, "set environment variables (KEY=VALUE, foreground command only)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read environment variables from a file (KEY=VALUE per line, foreground command only)")
	cmd.Flags().StringVarP(&user, "user", "u", "", "run as user (foreground command only)")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the VM (foreground command only)")

	return cmd
}

func runRun(store *state.Store, image string, cmdArgs []string, nameFlag string, detach bool, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, interactive bool, workdir string, envVars []string, user string) error {
	resolvedImage := resolveImage(image)

	// runStartAppleVZ doesn't accept an image parameter at all today — a
	// pre-existing gap in `mvm start --image` on applevz. Fail clearly here
	// rather than silently booting the default rootfs for a request that
	// named something else. A backend-load error must surface here rather
	// than default-guessing "firecracker" (GetBackend's behavior): guessing
	// wrong would skip this guard entirely and let a custom-image request
	// silently boot the default rootfs on applevz instead. Hence
	// GetBackendE, not GetBackend — see store.go's GetBackend doc comment.
	if resolvedImage != "" {
		backend, err := store.GetBackendE()
		if err != nil {
			return fmt.Errorf("read backend: %w", err)
		}
		if backend == "applevz" {
			return fmt.Errorf("custom images are not supported on the Apple VZ backend yet (only the default \"base\" image); got %q", image)
		}
	}

	existing, err := existingVMNames(store)
	if err != nil {
		return err
	}
	name, ephemeral := resolveRunName(nameFlag, existing)

	// Always create detached — run manages its own foreground behavior
	// (readiness wait + exec) rather than delegating to start's boot-log
	// streaming.
	if err := runStart(store, name, true, ports, netPolicy, volumes, "", "", cpus, memoryMB, resolvedImage, false, nil, nil); err != nil {
		return fmt.Errorf("start %q: %w", name, err)
	}

	cleanup := func() {
		if ephemeral {
			if err := runDelete(store, name, true); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to clean up ephemeral VM %q: %v\n", name, err)
			}
		}
	}

	if detach {
		if ephemeral {
			fmt.Printf("%s (ephemeral — clean up with: mvm delete %s)\n", name, name)
		} else {
			fmt.Printf("%s\n", name)
		}
		return nil
	}

	// Neither backend's create path blocks until the guest agent is
	// reachable, so a command run immediately after create can race a cold
	// boot (~3s). Probe with a silent no-op exec until the VM responds.
	if err := waitForReady(30*time.Second, func() error {
		return runExec(store, name, []string{"true"}, false, false, "", nil, "")
	}); err != nil {
		cleanup()
		return fmt.Errorf("VM %q never became ready: %w", name, err)
	}

	if len(cmdArgs) == 0 {
		cmdArgs = []string{"/bin/bash"}
		interactive = true
	}

	execErr := runExec(store, name, cmdArgs, interactive, false, workdir, envVars, user)
	cleanup()
	return execErr
}
