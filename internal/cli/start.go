package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newStartCmd(store *state.Store) *cobra.Command {
	var (
		detach    bool
		ports     []string
		netPolicy string
		volumes   []string
		seccomp   string
		watch     string
		cpus      int
		memoryMB  int
		image     string
		jsonOut   bool
		startup   string
		secretsF  []string
		rm        bool
	)

	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Create and boot a new microVM",
		Long: `Create and boot a new microVM.

  mvm start my-app
  mvm start my-app -p 8080:80           # forward host:8080 to guest:80
  mvm start my-app -p 3000:3000 -p 5432:5432
  mvm start my-app --net-policy deny     # block all outbound traffic
  mvm start my-app --net-policy allow:github.com,npmjs.org
  mvm start my-app -v ./src:/app         # mount host dir into guest
  mvm start my-app --seccomp strict      # restrict syscalls
  mvm start my-app --watch ./src         # rebuild on file changes
  mvm start my-app --cpus 4 --memory 2048  # custom resources
  mvm start my-app --image my-image       # use custom rootfs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateStartRM(rm); err != nil {
				return err
			}
			portMaps, err := parsePorts(ports)
			if err != nil {
				return err
			}
			volumes, err = parseVolumes(volumes)
			if err != nil {
				return err
			}
			var spec *StartupSpec
			if startup != "" {
				spec, err = loadStartupSpec(startup)
				if err != nil {
					return err
				}
			}
			return runStart(store, args[0], detach, portMaps, netPolicy, volumes, seccomp, watch, cpus, memoryMB, image, jsonOut, spec, secretsF, false)
		},
	}

	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "detach: don't stream boot output, return immediately after VM starts")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit a structured JSON result with boot path and per-phase timing")
	cmd.Flags().StringVar(&startup, "startup", "", "JSON startup recipe: git clone + commands + ready-check (applevz)")
	cmd.Flags().StringArrayVar(&secretsF, "secret", nil, "attach a stored secret, injected per-exec (repeatable; applevz)")
	cmd.Flags().StringArrayVarP(&ports, "publish", "p", nil, "publish port (hostPort:guestPort[/proto])")
	cmd.Flags().StringVar(&netPolicy, "net-policy", "open", "network policy: open, deny, or allow:domain1,domain2")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "V", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().StringVar(&seccomp, "seccomp", "", "seccomp profile: strict, moderate, or permissive")
	cmd.Flags().StringVar(&watch, "watch", "", "watch directory for changes and sync to guest")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVar(&memoryMB, "memory", 0, "RAM in MiB (default: 1024)")
	cmd.Flags().StringVar(&image, "image", "", "custom rootfs image name (built with mvm build)")
	cmd.Flags().BoolVar(&rm, "rm", false, "not supported on start — use mvm run instead")

	return cmd
}

func parsePorts(ports []string) ([]state.PortMap, error) {
	var result []state.PortMap
	for _, p := range ports {
		proto := "tcp"
		if idx := strings.Index(p, "/"); idx != -1 {
			proto = p[idx+1:]
			p = p[:idx]
		}
		parts := strings.Split(p, ":")

		var hostIP, hostPortStr, guestPortStr string
		switch len(parts) {
		case 2:
			hostPortStr, guestPortStr = parts[0], parts[1]
		case 3:
			hostIP, hostPortStr, guestPortStr = parts[0], parts[1], parts[2]
			if hostIP == "" {
				return nil, fmt.Errorf("invalid port format %q (empty host-ip; use hostPort:guestPort to bind the backend's default address)", p)
			}
		default:
			return nil, fmt.Errorf("invalid port format %q (expected [host-ip:]hostPort:guestPort)", p)
		}

		host, err := strconv.Atoi(hostPortStr)
		if err != nil {
			return nil, fmt.Errorf("invalid host port %q: %w", hostPortStr, err)
		}
		guest, err := strconv.Atoi(guestPortStr)
		if err != nil {
			return nil, fmt.Errorf("invalid guest port %q: %w", guestPortStr, err)
		}
		result = append(result, state.PortMap{HostIP: hostIP, HostPort: host, GuestPort: guest, Proto: proto})
	}
	return result, nil
}

// parseVolumes validates each "hostPath:guestPath" entry and resolves a
// relative hostPath to an absolute one against the CLI process's own cwd.
//
// Absolutizing here (not deeper in the stack) matters because both backends
// eventually need an unambiguous path: on Firecracker, the daemon that reads
// hostPath runs inside the Lima VM, which only sees paths under $HOME (Lima's
// own mount config — see internal/lima/lima.go's ".mounts" setting); on
// applevz, the Swift helper's VZSharedDirectory resolves a relative hostPath
// against its own process cwd, not the user's, which is never what's wanted.
// This does NOT resolve the "remote daemon" case (CLI and daemon on
// different machines) — see the plan's Global Constraints.
//
// guestPath must be absolute: it becomes a mount target (applevz) or a tar
// extraction directory (Firecracker), and a relative guest path is ambiguous
// once the guest's cwd for that operation isn't guaranteed (root's home
// varies; no shell profile has run yet at mount time).
func parseVolumes(volumes []string) ([]string, error) {
	var result []string
	for _, v := range volumes {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid volume format %q (expected hostPath:guestPath)", v)
		}
		hostPath, guestPath := parts[0], parts[1]
		if !filepath.IsAbs(guestPath) {
			return nil, fmt.Errorf("invalid volume %q: guest path %q must be absolute", v, guestPath)
		}
		if !filepath.IsAbs(hostPath) {
			abs, err := filepath.Abs(hostPath)
			if err != nil {
				return nil, fmt.Errorf("resolve host path %q: %w", hostPath, err)
			}
			hostPath = abs
		}
		result = append(result, hostPath+":"+guestPath)
	}
	return result, nil
}

// validateStartRM rejects --rm on `mvm start`. start has no foreground
// lifetime to key cleanup on (it returns right after boot, with no
// "the command that was running" to wait for) — silently reinterpreting
// --rm as "delete on `mvm stop`" would bolt an undocumented lifecycle rule
// onto a command whose whole contract is idempotent, durable upsert (see
// the design spec's decision #5: "start is upsert, forever"). mvm run
// already has the correct ephemeral-unless---name semantics, so this
// points there instead of inventing a second meaning for --rm.
func validateStartRM(rm bool) error {
	if rm {
		return fmt.Errorf("mvm start does not support --rm: start has no foreground command to key cleanup on. Use `mvm run <image>` instead — it deletes the VM automatically unless you pass --name")
	}
	return nil
}

func runStart(store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, watch string, cpus, memoryMB int, image string, jsonOut bool, startup *StartupSpec, secretNames []string, quiet bool) error {
	// Merge secrets from the startup spec, then validate they all exist up front
	// (a typo'd secret should fail the start, not silently inject nothing).
	if startup != nil {
		secretNames = append(secretNames, startup.Secrets...)
	}
	if err := validateSecretsExist(secretNames); err != nil {
		return err
	}

	// Cloud/remote mode: the local state doesn't matter — the daemon is
	// the source of truth. Skip the local init check entirely.
	if os.Getenv("MVM_REMOTE") != "" {
		return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image, startup, secretNames, quiet)
	}

	initialized, err := store.IsInitialized()
	if err != nil {
		return err
	}
	if !initialized {
		return fmt.Errorf("mvm is not initialized. Run: mvm init")
	}

	backend := store.GetBackend()

	// Apple VZ path — dispatch to separate function
	if backend == "applevz" {
		out := resolveOutputMode(jsonOut, quiet)
		_, err := runStartAppleVZ(store, name, detach, ports, netPolicy, cpus, memoryMB, volumes, out, startup, secretNames, image)
		return err
	}
	// Firecracker path: route through daemon
	return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image, startup, secretNames, quiet)
}

// runStartViaDaemon creates a VM by calling the daemon's /vms endpoint.
// Used for both local-mode (Unix socket) and cloud-mode (TCP+TLS). quiet
// suppresses the boot banner entirely — used by mvm run's path, which
// prints its own status instead (see run.go). There is no JSON output
// mode on this path yet (a pre-existing, separate gap: `mvm start --json`
// on the firecracker/daemon backend silently falls back to the human
// banner, unlike the applevz path) — out of scope here; quiet only adds
// "print nothing" alongside the existing "print the human banner".
//
// Note: quiet gates BEFORE the startup-recipe logic below — today, mvm run
// (the only quiet=true caller) has no --startup flag, so quiet and startup
// never co-occur in practice, but if they ever did the recipe would be
// skipped along with the banner. This preserves quiet's pre-existing,
// already-reviewed behavior unchanged rather than special-casing it.
func runStartViaDaemon(name string, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, cpus, memoryMB int, image string, startup *StartupSpec, secretNames []string, quiet bool) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()
	resp, err := sc.CreateVM(ctx, server.CreateVMRequest{
		Name:      name,
		Cpus:      cpus,
		MemoryMB:  memoryMB,
		Ports:     ports,
		NetPolicy: netPolicy,
		Volumes:   volumes,
		Seccomp:   seccomp,
		Image:     image,
		// Only secret NAMES cross this boundary — see the package-level
		// security invariant in this plan's Global Constraints. Values are
		// decrypted client-side, per-exec, exactly like runExecAppleVZ does
		// for applevz (internal/cli/exec.go).
		Secrets: secretNames,
	})
	if err != nil {
		return err
	}

	if quiet {
		return nil
	}

	fmt.Printf("\n  %s is running!\n", resp.Name)
	fmt.Printf("    IP:   %s\n", resp.GuestIP)
	for _, p := range resp.Ports {
		host := p.HostIP
		if host == "" {
			host = "localhost"
		}
		fmt.Printf("    Port: %s:%d -> %s:%d/%s\n", host, p.HostPort, resp.GuestIP, p.GuestPort, p.Proto)
	}
	fmt.Printf("    Exec: mvm exec %s -- <command>\n", resp.Name)

	if startup == nil {
		return nil
	}

	// Merge attached secrets into the recipe's env, decrypted from host
	// memory here — mirrors the identical block in runStartAppleVZ below.
	if env, err := secretEnvVars(secretNames); err != nil {
		return fmt.Errorf("load secrets for startup recipe: %w", err)
	} else if len(env) > 0 {
		if startup.Env == nil {
			startup.Env = map[string]string{}
		}
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i > 0 {
				startup.Env[kv[:i]] = kv[i+1:]
			}
		}
	}

	fmt.Printf("  Waiting for guest agent before running startup recipe...\n")
	if err := waitForReady(60*time.Second, func() error {
		_, _, err := sc.Exec(ctx, name, "true")
		return err
	}); err != nil {
		return fmt.Errorf("VM %q never became ready for the startup recipe: %w", name, err)
	}

	timer := newPhaseTimer()
	logf := func(format string, a ...any) { fmt.Printf(format, a...) }
	if err := runStartupRecipe(ctx, daemonRecipeAgent{sc: sc, vmName: name}, startup, timer, logf); err != nil {
		fmt.Printf("    Startup recipe failed: %v\n", err)
		return err
	}
	return nil
}

func printPorts(vm *state.VM) {
	for _, p := range vm.Ports {
		host := p.HostIP
		if host == "" {
			host = "localhost"
		}
		fmt.Printf("    Port: %s:%d -> %s:%d/%s\n", host, p.HostPort, vm.GuestIP, p.GuestPort, p.Proto)
	}
}

// applevzSpec records the applevz create request as a declarative spec
// (design spec §4: persisted verbatim, returned by inspect). Image and
// Seccomp stay empty — the applevz path supports neither yet.
func applevzSpec(ports []state.PortMap, netPolicy string, cpus, memoryMB int, volumes []string, startup *StartupSpec, secretNames []string) *state.VMSpec {
	spec := &state.VMSpec{
		Cpus:      cpus,
		MemoryMB:  memoryMB,
		Ports:     ports,
		Volumes:   volumes,
		NetPolicy: netPolicy,
		Secrets:   secretNames,
	}
	if startup != nil {
		if raw, err := json.Marshal(startup); err == nil {
			spec.Startup = raw
		}
	}
	return spec
}

// virtiofsMountCommands returns, in order, the shell command that mounts
// each already-validated "hostPath:guestPath" volume inside the guest via
// virtio-fs. Tags are assigned "vol0", "vol1", ... by position — this must
// match vz/Sources/mvm-vz/Commands/Create.swift's share-parsing loop exactly,
// since the tag is never threaded back through the mvm-vz status line; both
// sides derive it independently from the same ordering.
func virtiofsMountCommands(volumes []string) ([]string, error) {
	var cmds []string
	for i, v := range volumes {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid volume format %q (expected hostPath:guestPath)", v)
		}
		guestPath := parts[1]
		tag := fmt.Sprintf("vol%d", i)
		cmds = append(cmds, fmt.Sprintf("mkdir -p %s && mount -t virtiofs %s %s",
			shellQuote(guestPath), tag, shellQuote(guestPath)))
	}
	return cmds, nil
}

// resolveApplevzKernel picks the kernel to boot the applevz backend with.
//
// Preferred: cacheDir/vmlinux-applevz, a custom-built kernel with virtio-fs
// support (see build-applevz-kernel.sh) required for -V volume mounts.
//
// That kernel is NOT part of a fresh `mvm init --backend applevz` install —
// runInitAppleVZ only ever downloads the shared cacheDir/vmlinux — so on any
// machine where nobody has manually run build-applevz-kernel.sh, the custom
// kernel won't exist. Falling back to the shared vmlinux there keeps the
// backend bootable; -V volume mounts still degrade gracefully afterward via
// virtiofsMountCommands' own per-mount warning, since the shared kernel has
// no virtio-fs driver.
//
// Returns the kernel path to boot with, and — only when falling back — a
// warning message for the caller to log.
func resolveApplevzKernel(cacheDir string) (kernelPath string, warning string) {
	custom := filepath.Join(cacheDir, "vmlinux-applevz")
	if _, err := os.Stat(custom); err == nil {
		return custom, ""
	}
	shared := filepath.Join(cacheDir, "vmlinux")
	return shared, fmt.Sprintf(
		"custom applevz kernel not found at %s; falling back to the shared vmlinux. "+
			"Volume mounts (-V) will not work until you build it: internal/firecracker/scripts/build-applevz-kernel.sh",
		custom,
	)
}

// imageFileName maps an --image value to its rootfs filename, matching the
// Firecracker path's convention (firecracker.CacheDir()+"/"+name+".ext4",
// internal/firecracker/config.go:229). image == "" means the implicit
// default.
func imageFileName(image string) string {
	if image == "" {
		return "base.ext4"
	}
	return image + ".ext4"
}

// resolveAppleVZImage returns the local rootfs path for image inside
// cacheDir, fetching it via fetch first if it isn't already cached locally.
// fetch is injected so this is testable without a real daemon; runStartAppleVZ
// passes a closure around requireDaemon()+Client.DownloadImage. A nil fetch
// with a missing image is a clear, immediate error rather than a nil-pointer
// call.
func resolveAppleVZImage(cacheDir, image string, fetch func(image, destPath string) error) (string, error) {
	rootfsPath := filepath.Join(cacheDir, imageFileName(image))
	if image == "" {
		return rootfsPath, nil
	}
	if _, err := os.Stat(rootfsPath); err == nil {
		return rootfsPath, nil
	}
	if fetch == nil {
		return "", fmt.Errorf("image %q not found in %s and no daemon reachable to fetch it (build it with: mvm build -t %s -f <Dockerfile>)", image, cacheDir, image)
	}
	if err := fetch(image, rootfsPath); err != nil {
		return "", fmt.Errorf("fetch image %q from daemon: %w", image, err)
	}
	return rootfsPath, nil
}

// runStartAppleVZ starts a VM using the Apple Virtualization.framework backend.
//
// As of PR #2 this path drives the in-guest agent over vsock via the
// per-VM mvm-vz helper IPC socket — no SSH, no TAP-IP TCP, no Lima.
// The previous SSH-based post-boot path (and the applyPostBootDirect
// helper that went with it) has been removed.
func runStartAppleVZ(store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, cpus, memoryMB int, volumes []string, out outputMode, startup *StartupSpec, secretNames []string, image string) (*BootResult, error) {
	// Progress goes to stderr unless we're in human mode, so stdout stays a
	// clean JSON object (or silent, for the bench harness).
	logf := func(format string, a ...any) {
		if out == outHuman {
			fmt.Printf(format, a...)
		} else {
			fmt.Fprintf(os.Stderr, format, a...)
		}
	}
	timer := newPhaseTimer()

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".mvm", "cache")
	// applevz prefers its own kernel (vmlinux-applevz) built with virtio-fs
	// support for `-V` volume mounts, falling back to the shared vmlinux (no
	// virtio-fs driver) when the custom kernel hasn't been built. The
	// Firecracker backend always uses the shared vmlinux. See
	// resolveApplevzKernel and build-applevz-kernel.sh.
	kernelPath, kernelWarning := resolveApplevzKernel(cacheDir)
	if kernelWarning != "" {
		logf("  Warning: %s\n", kernelWarning)
	}
	rootfsPath, err := resolveAppleVZImage(cacheDir, image, func(img, dest string) error {
		sc, dErr := requireDaemon()
		if dErr != nil {
			return dErr
		}
		logf("  Image %q not cached locally, fetching from daemon...\n", img)
		return sc.DownloadImage(context.Background(), img, dest)
	})
	if err != nil {
		return nil, err
	}
	timer.mark("image_resolve")

	vmDir := filepath.Join(home, ".mvm", "vms", name)
	os.MkdirAll(vmDir, 0o755)
	vmRootfs := filepath.Join(vmDir, "rootfs.ext4")
	statePath := filepath.Join(vmDir, "state.vzvmsave")

	now := time.Now()
	var netIndex int
	if existing, _ := store.GetVM(name); existing != nil {
		// Already in state — allow it only as a restore: a stopped VM that has
		// a saved state file. Reuse its existing network allocation.
		if _, statErr := os.Stat(statePath); statErr != nil {
			return nil, fmt.Errorf("microVM %q already exists", name)
		}
		netIndex = existing.NetIndex
		// A clean `mvm stop` always clears ForwarderPID; a nonzero value here
		// means a previous run's forwarder process leaked (e.g. a crash).
		// Kill it defensively before the new run spawns a fresh one, so we
		// never end up with two processes fighting over the same host port.
		killForwarder(store, name, existing.ForwarderPID)
		store.UpdateVM(name, func(v *state.VM) { v.Status = "starting" })
	} else {
		vmEntry := &state.VM{
			Name:      name,
			Status:    "starting",
			Backend:   "applevz",
			Ports:     ports,
			NetPolicy: netPolicy,
			CreatedAt: now,
		}
		var err error
		netIndex, err = store.ReserveVM(vmEntry)
		if err != nil {
			return nil, err
		}
	}
	alloc := state.AllocateNet(netIndex)

	// If a saved snapshot exists, restore from it. The rootfs already holds the
	// writes from before the save, so we must NOT re-copy base.ext4 over it.
	restoreFrom := ""
	bootPath := BootCold
	if _, statErr := os.Stat(statePath); statErr == nil {
		restoreFrom = statePath
		bootPath = BootRestore
		// Roll the disk back to the checkpoint snapshot — the saved memory state
		// expects the disk contents from save time. (No-op if the disk is
		// unchanged; undoes post-checkpoint filesystem writes if it changed.)
		diskSnap := filepath.Join(vmDir, "rootfs.snapshot.ext4")
		if _, e := os.Stat(diskSnap); e == nil {
			_ = os.Remove(vmRootfs)
			if err := execLocal(fmt.Sprintf("cp -c %s %s", diskSnap, vmRootfs)); err != nil {
				if err := execLocal(fmt.Sprintf("cp %s %s", diskSnap, vmRootfs)); err != nil {
					return nil, fmt.Errorf("restore disk snapshot: %w", err)
				}
			}
		}
	} else if err := execLocal(fmt.Sprintf("cp -c %s %s", rootfsPath, vmRootfs)); err != nil {
		// APFS copy-on-write clone — instant regardless of rootfs size, so a
		// richer (Node/Python) base doesn't slow cold boot. Fall back to a plain
		// copy on non-APFS / cross-device.
		if err := execLocal(fmt.Sprintf("cp %s %s", rootfsPath, vmRootfs)); err != nil {
			store.RemoveVM(name)
			return nil, fmt.Errorf("copy rootfs: %w", err)
		}
	}
	timer.mark("disk_prep")

	bootArgs := "console=hvc0 root=/dev/vda rw reboot=k panic=1 rootfstype=ext4 init=/sbin/mvm-init ip=dhcp"

	vzBackend := vm.NewAppleVZBackend(filepath.Join(home, ".mvm"))

	if restoreFrom != "" {
		logf("Restoring microVM '%s' from saved state (Apple VZ)...\n", name)
	} else {
		logf("Starting microVM '%s' (Apple VZ)...\n", name)
	}

	vzCpus := cpus
	if vzCpus <= 0 {
		vzCpus = 2
	}
	vzMem := memoryMB
	if vzMem <= 0 {
		vzMem = 1024
	}
	startResult, err := vzBackend.StartVM(name, kernelPath, vmRootfs, bootArgs, alloc.GuestMAC, vzCpus, vzMem, volumes, restoreFrom)
	if err != nil {
		store.RemoveVM(name)
		return nil, fmt.Errorf("start VM: %w", err)
	}
	pid := startResult.PID
	timer.mark("vmm_spawn")

	// GuestIP/TAPIP are not yet known: the guest gets its address via
	// kernel-level DHCP against Apple's VZNAT device (see the bootArgs
	// comment above), not the static internal/state.AllocateNet scheme, so
	// there's nothing to record until the guest agent self-reports what it
	// was actually handed (below, once agentReady).
	if err := store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.TAPDevice = ""
		v.GuestMAC = alloc.GuestMAC
		v.PID = pid
		v.RootfsPath = vmRootfs
		v.Backend = "applevz"
		v.Secrets = secretNames
		v.Spec = applevzSpec(ports, netPolicy, cpus, memoryMB, volumes, startup, secretNames)
	}); err != nil {
		store.RemoveVM(name)
		return nil, err
	}

	updatedVM := &state.VM{
		Name:       name,
		Status:     "running",
		GuestMAC:   alloc.GuestMAC,
		PID:        pid,
		RootfsPath: vmRootfs,
		Backend:    "applevz",
		Ports:      ports,
		NetPolicy:  netPolicy,
	}

	// Wait for the in-guest agent to be reachable over vsock. The helper
	// IPC socket was already bound before StartVM returned (we read the
	// status line), so any failure here is the agent not being up yet.
	agent := vzBackend.AgentClient(name)
	logf("  Waiting for guest agent...\n")
	agentReady := waitForAgent(agent, 60*time.Second)
	timer.mark("agent_ready")

	if agentReady {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// DNS: point the guest at a real resolver. (The default route itself
		// is already set — the kernel's own ip=dhcp autoconfig installs it
		// from the DHCP-offered gateway before userspace even starts; no
		// "ip route add" is needed or possible, since the guest image ships
		// no ip/ifconfig binary at all.)
		if _, err := agent.Exec(ctx, "echo 'nameserver 8.8.8.8' > /etc/resolv.conf", ""); err != nil {
			logf("  Warning: configure DNS: %v\n", err)
		}

		// Discover what address DHCP actually handed out. Apple's
		// VZNATNetworkDeviceAttachment runs its own DHCP pool (observed
		// 192.168.65.0/24 on this host, gateway 192.168.65.1 — the subnet is
		// Apple's to choose and may differ per machine); the host has no way
		// to know the assigned address except asking the guest.
		if info, err := agent.NetInfo(ctx); err != nil {
			logf("  Warning: discover guest IP: %v\n", err)
		} else {
			// TAPIP is the Firecracker-backend name for "the address the
			// guest reaches the host at" (the tap device's host-side IP);
			// reused here for the applevz backend's NAT gateway, which
			// plays the identical role — see the guest→host address note
			// in the -p forwarding help text below.
			guestIP, hostAddr := info.IP, info.Gateway
			store.UpdateVM(name, func(v *state.VM) {
				v.GuestIP = guestIP
				v.TAPIP = hostAddr
			})
			updatedVM.GuestIP = guestIP
			updatedVM.TAPIP = hostAddr
		}

		// Apply network policy via the agent.
		if err := applyVZNetworkPolicy(ctx, agent, netPolicy); err != nil {
			logf("  Warning: apply network policy: %v\n", err)
		}

		// Mount each -V share via virtio-fs. Depends on the tags assigned
		// in vz/Sources/mvm-vz/Commands/Create.swift's share-parsing loop
		// matching this exact order — see virtiofsMountCommands's comment.
		if mountCmds, err := virtiofsMountCommands(volumes); err != nil {
			logf("  Warning: invalid volume spec: %v\n", err)
		} else {
			for _, mc := range mountCmds {
				if _, err := agent.Exec(ctx, mc, ""); err != nil {
					logf("  Warning: mount volume: %v\n", err)
				}
			}
		}
		timer.mark("net_setup")

		// Real host->guest TCP forwarding for -p. Spawned as a detached
		// process so the listeners outlive this `mvm start` invocation; see
		// forward_daemon.go. Runs over the same vsock tcp_forward channel
		// `mvm preview` already uses — independent of guest IP networking
		// (Bug 1), so this works even if DHCP discovery above failed.
		if len(ports) > 0 {
			if fpid, err := spawnPortForwarders(name); err != nil {
				logf("  Warning: port forwarding: %v\n", err)
				if fpid > 0 {
					store.UpdateVM(name, func(v *state.VM) { v.ForwarderPID = fpid })
				}
			} else {
				store.UpdateVM(name, func(v *state.VM) { v.ForwarderPID = fpid })
			}
		}
	}

	// Run the declarative startup recipe (git clone + commands + ready check)
	// over the agent. Uses its own context — clones/installs run far longer
	// than the 30s net-setup window. A failure is surfaced but the VM stays up
	// so it can be inspected.
	var startupErr error
	if agentReady && startup != nil {
		// Make attached secrets available to startup commands too (decrypted
		// from host memory here; never written to a guest file).
		if env, err := secretEnvVars(secretNames); err != nil {
			startupErr = err
		} else if len(env) > 0 {
			if startup.Env == nil {
				startup.Env = map[string]string{}
			}
			for _, kv := range env {
				if i := strings.IndexByte(kv, '='); i > 0 {
					startup.Env[kv[:i]] = kv[i+1:]
				}
			}
		}
		if startupErr == nil {
			startupErr = runStartupRecipe(context.Background(), applevzRecipeAgent{agent}, startup, timer, logf)
		}
	}

	result := &BootResult{
		Name:       name,
		Backend:    "applevz",
		BootPath:   bootPath,
		GuestIP:    updatedVM.GuestIP,
		AgentReady: agentReady,
		TotalMs:    timer.totalMs(),
		Phases:     timer.phases,
	}

	switch out {
	case outJSON:
		json.NewEncoder(os.Stdout).Encode(result)
		return result, startupErr
	case outQuiet:
		return result, startupErr
	}

	if agentReady {
		fmt.Printf("\n  %s is running! (Apple VZ)\n", name)
		fmt.Printf("    IP:   %s\n", updatedVM.GuestIP)
		if updatedVM.TAPIP != "" {
			fmt.Printf("    Host: %s (reach the host from inside the guest at this address)\n", updatedVM.TAPIP)
		}
		printPorts(updatedVM)
		fmt.Printf("    Boot: %s in %.0fms\n", bootPath, result.TotalMs)
		fmt.Printf("    Exec: mvm exec %s -- <command>\n", name)
	} else {
		fmt.Printf("\n  %s started but agent not reachable yet.\n", name)
		fmt.Printf("    Exec: mvm exec %s -- <command>  (when ready)\n", name)
	}
	if startupErr != nil {
		fmt.Printf("    Startup recipe failed: %v\n", startupErr)
	}
	return result, startupErr
}

// waitForAgent polls the agent client until Ping succeeds or the deadline
// is hit.
func waitForAgent(c *agentclient.Client, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := c.Ping(ctx)
		cancel()
		if err == nil {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// applyVZNetworkPolicy enforces a network policy by issuing iptables rules
// inside the guest via the agent. This is the same shape as the FC path
// in internal/firecracker/process.go ApplyNetworkPolicyViaAgent — a
// follow-up will move both to a host-side packet filter.
func applyVZNetworkPolicy(ctx context.Context, agent *agentclient.Client, netPolicy string) error {
	if netPolicy == "" || netPolicy == "open" {
		return nil
	}
	var rules string
	switch {
	case netPolicy == "deny":
		rules = "iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT; " +
			"iptables -A OUTPUT -p udp --dport 53 -j ACCEPT; " +
			"iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT; " +
			"iptables -A OUTPUT -o lo -j ACCEPT; " +
			"iptables -A OUTPUT -j DROP"
	case strings.HasPrefix(netPolicy, "allow:"):
		domains := strings.TrimPrefix(netPolicy, "allow:")
		rules = "iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT; " +
			"iptables -A OUTPUT -p udp --dport 53 -j ACCEPT; " +
			"iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT; " +
			"iptables -A OUTPUT -o lo -j ACCEPT"
		for _, domain := range strings.Split(domains, ",") {
			domain = strings.TrimSpace(domain)
			if domain != "" {
				rules += fmt.Sprintf("; for ip in $(getent hosts %s 2>/dev/null | awk '{print $1}'); do iptables -A OUTPUT -d $ip -j ACCEPT; done", domain)
			}
		}
		rules += "; iptables -A OUTPUT -j DROP"
	default:
		return fmt.Errorf("unknown network policy: %s", netPolicy)
	}
	_, err := agent.Exec(ctx, rules, "")
	return err
}

// execLocal is defined in init.go
