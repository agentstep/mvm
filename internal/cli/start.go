package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"os"
	"os/exec"
	"path/filepath"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newStartCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	var (
		detach    bool
		ports     []string
		netPolicy string
		volumes   []string
		seccomp   string
		watch     string
		cpus      int
		memoryMB  int
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
  mvm start my-app --cpus 4 --memory 2048  # custom resources`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			portMaps, err := parsePorts(ports)
			if err != nil {
				return err
			}
			return runStart(limaClient, store, args[0], detach, portMaps, netPolicy, volumes, seccomp, watch, cpus, memoryMB)
		},
	}

	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "detach: don't stream boot output, return immediately after VM starts")
	cmd.Flags().StringArrayVarP(&ports, "publish", "p", nil, "publish port (hostPort:guestPort[/proto])")
	cmd.Flags().StringVar(&netPolicy, "net-policy", "open", "network policy: open, deny, or allow:domain1,domain2")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "V", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().StringVar(&seccomp, "seccomp", "", "seccomp profile: strict, moderate, or permissive")
	cmd.Flags().StringVar(&watch, "watch", "", "watch directory for changes and sync to guest")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVar(&memoryMB, "memory", 0, "RAM in MiB (default: 1024)")

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
		parts := strings.SplitN(p, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid port format %q (expected hostPort:guestPort)", p)
		}
		host, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid host port %q: %w", parts[0], err)
		}
		guest, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid guest port %q: %w", parts[1], err)
		}
		result = append(result, state.PortMap{HostPort: host, GuestPort: guest, Proto: proto})
	}
	return result, nil
}

func runStart(limaClient *lima.Client, store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, watch string, cpus, memoryMB int) error {
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
		return runStartAppleVZ(store, name, detach, ports, netPolicy, cpus, memoryMB, volumes)
	}

	// Fast path: route through daemon (direct syscalls inside Lima, no SSH)
	sc := server.DefaultClient()
	if sc.IsAvailable() {
		resp, err := sc.CreateVM(context.Background(), server.CreateVMRequest{
			Name:      name,
			Cpus:      cpus,
			MemoryMB:  memoryMB,
			Ports:     ports,
			NetPolicy: netPolicy,
		})
		if err == nil {
			fmt.Printf("\n  %s is running!\n", resp.Name)
			fmt.Printf("    IP:   %s\n", resp.GuestIP)
			fmt.Printf("    Exec: mvm exec %s -- <command>\n", resp.Name)
			return nil
		}
		// Daemon failed — fall through to direct path
	}

	// Firecracker path — ensure Lima is running
	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	// Atomically reserve name + net index before doing any work.
	// This prevents two concurrent `mvm start` from picking the same slot.
	now := time.Now()
	vm := &state.VM{
		Name:      name,
		Status:    "starting",
		Ports:     ports,
		NetPolicy: netPolicy,
		Cpus:      cpus,
		MemoryMB:  memoryMB,
		CreatedAt: now,
	}
	netIndex, err := store.ReserveVM(vm)
	if err != nil {
		return err
	}
	alloc := state.AllocateNet(netIndex)

	fmt.Printf("Starting microVM '%s'...\n", name)

	// Declare before cleanup closure so it captures by reference
	var pid int
	var socketPath string
	fromPool := false

	// If anything fails after reservation, clean up state + any running resources
	cleanup := func() {
		cleanupVM := &state.VM{
			Name:       name,
			PID:        pid,
			SocketPath: socketPath,
			TAPDevice:  alloc.TAPDev,
		}
		firecracker.Cleanup(limaClient, cleanupVM)
		store.RemoveVM(name)
	}

	// Only claim from pool if using default resources (pool VMs have fixed config)
	usePool := (cpus <= 0 || cpus == firecracker.GuestVcpuCount) && (memoryMB <= 0 || memoryMB == firecracker.GuestMemSizeMiB)
	if usePool {
		var claimErr error
		var claimedSocket string
		pid, claimedSocket, claimErr = firecracker.ClaimPoolSlot(limaClient, name, alloc)
		if claimErr == nil && pid > 0 {
			fromPool = true
			socketPath = claimedSocket
			fmt.Println("  Claimed from warm pool (instant)")
			firecracker.ReplenishPool(limaClient)
		}
	}

	if !fromPool {
		socketPath = firecracker.SocketPath(name)
		useSnapshot := firecracker.HasSnapshot(limaClient)
		if useSnapshot {
			pid, err = firecracker.StartFromSnapshot(limaClient, name, alloc)
			if err != nil {
				useSnapshot = false
			}
		}
		if !useSnapshot {
			pid, err = firecracker.Start(limaClient, name, alloc, cpus, memoryMB)
			if err != nil {
				cleanup()
				return err
			}
		}
	}

	// Update the reservation with runtime details
	if err := store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = alloc.TAPDev
		v.GuestMAC = alloc.GuestMAC
		v.SocketPath = socketPath
		v.PID = pid
		v.RootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"
	}); err != nil {
		cleanup()
		return err
	}

	// Refresh the local vm with the fields we just persisted
	vm.Status = "running"
	vm.GuestIP = alloc.GuestIP
	vm.TAPIP = alloc.TAPIP
	vm.TAPDevice = alloc.TAPDev
	vm.GuestMAC = alloc.GuestMAC
	vm.SocketPath = socketPath
	vm.PID = pid
	vm.RootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"

	// Apply post-boot configuration (same for pool and fresh boot)
	applyPostBoot := func() {
		firecracker.SetupPortForwarding(limaClient, vm)
		firecracker.ApplyNetworkPolicyViaAgent(limaClient, vm)
		firecracker.SetupVolumeMounts(limaClient, vm, volumes)
		firecracker.ApplySeccompViaAgent(limaClient, vm, seccomp)
	}

	if fromPool {
		applyPostBoot()
		fmt.Printf("\n  %s is running!\n", name)
		fmt.Printf("    IP:   %s\n", alloc.GuestIP)
		printPorts(vm)
		fmt.Printf("    SSH:  mvm ssh %s\n", name)
		if watch != "" {
			return runWatch(limaClient, vm, watch)
		}
		return nil
	}

	// Apply post-boot config in background (don't block on guest readiness)
	go func() {
		if firecracker.WaitForGuest(limaClient, alloc.GuestIP, 120*time.Second) {
			firecracker.SetupGuestNetworkViaAgent(limaClient, alloc.GuestIP, alloc.TAPIP)
			applyPostBoot()
		}
	}()

	fmt.Printf("\n  %s is running!\n", name)
	fmt.Printf("    IP:   %s\n", alloc.GuestIP)
	printPorts(vm)
	fmt.Printf("    Exec: mvm exec %s -- <command>\n", name)

	return nil
}

func printPorts(vm *state.VM) {
	for _, p := range vm.Ports {
		fmt.Printf("    Port: localhost:%d -> %s:%d/%s\n", p.HostPort, vm.GuestIP, p.GuestPort, p.Proto)
	}
}

// runWatch watches a local directory for changes and syncs to the guest.
func runWatch(limaClient *lima.Client, vm *state.VM, dir string) error {
	if !isDirectory(dir) {
		return fmt.Errorf("watch path %q is not a directory", dir)
	}

	fmt.Printf("\n  Watching %s for changes (Ctrl-C to stop)...\n", dir)

	hash, err := hashDirectory(dir)
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	for {
		newHash, err := watchDirectory(dir, 1*time.Second, hash)
		if err != nil {
			return err
		}
		hash = newHash

		fmt.Printf("  Change detected, syncing to %s...\n", vm.Name)
		firecracker.SetupVolumeMounts(limaClient, vm, []string{dir + ":/app"})
		fmt.Println("  Synced.")
	}
}

// runStartAppleVZ starts a VM using the Apple Virtualization.framework backend.
func runStartAppleVZ(store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, cpus, memoryMB int, volumes []string) error {
	now := time.Now()
	vmEntry := &state.VM{
		Name:      name,
		Status:    "starting",
		Backend:   "applevz",
		Ports:     ports,
		NetPolicy: netPolicy,
		CreatedAt: now,
	}
	netIndex, err := store.ReserveVM(vmEntry)
	if err != nil {
		return err
	}
	alloc := state.AllocateNet(netIndex)

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".mvm", "cache")
	kernelPath := filepath.Join(cacheDir, "vmlinux")
	rootfsPath := filepath.Join(cacheDir, "base.ext4")

	// Copy rootfs for this VM
	vmDir := filepath.Join(home, ".mvm", "vms", name)
	os.MkdirAll(vmDir, 0o755)
	vmRootfs := filepath.Join(vmDir, "rootfs.ext4")

	// Sparse copy
	if err := execLocal(fmt.Sprintf("cp %s %s", rootfsPath, vmRootfs)); err != nil {
		store.RemoveVM(name)
		return fmt.Errorf("copy rootfs: %w", err)
	}

	bootArgs := fmt.Sprintf("console=hvc0 reboot=k panic=1 quiet random.trust_cpu=on rootfstype=ext4 ip=%s::%s:255.255.255.252::eth0:off",
		alloc.GuestIP, alloc.TAPIP)

	vzBackend := vm.NewAppleVZBackend(filepath.Join(home, ".mvm"))
	logPath := filepath.Join(vmDir, "console.log")

	fmt.Printf("Starting microVM '%s' (Apple VZ)...\n", name)

	vzCpus := cpus
	if vzCpus <= 0 {
		vzCpus = 2
	}
	vzMem := memoryMB
	if vzMem <= 0 {
		vzMem = 1024
	}
	pid, err := vzBackend.StartVM(name, kernelPath, vmRootfs, bootArgs, alloc.GuestMAC, vzCpus, vzMem, volumes)
	if err != nil {
		store.RemoveVM(name)
		return fmt.Errorf("start VM: %w", err)
	}

	_ = logPath

	if err := store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = ""
		v.GuestMAC = alloc.GuestMAC
		v.PID = pid
		v.RootfsPath = vmRootfs
		v.Backend = "applevz"
	}); err != nil {
		store.RemoveVM(name)
		return err
	}

	updatedVM := &state.VM{
		Name:      name,
		Status:    "running",
		GuestIP:   alloc.GuestIP,
		TAPIP:     alloc.TAPIP,
		GuestMAC:  alloc.GuestMAC,
		PID:       pid,
		RootfsPath: vmRootfs,
		Backend:   "applevz",
		Ports:     ports,
		NetPolicy: netPolicy,
	}

	// Wait for SSH to become ready (direct from macOS, no Lima)
	keyPath := filepath.Join(mvmDir, "keys", "mvm.id_ed25519")
	fmt.Println("  Waiting for SSH...")
	sshReady := false
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("ssh", "-i", keyPath,
			"-o", "ConnectTimeout=2", "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			fmt.Sprintf("root@%s", alloc.GuestIP), "echo SSH_OK")
		if out, err := cmd.Output(); err == nil && strings.Contains(string(out), "SSH_OK") {
			sshReady = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if sshReady {
		// Configure guest networking
		exec.Command("ssh", "-i", keyPath,
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			fmt.Sprintf("root@%s", alloc.GuestIP),
			fmt.Sprintf("ip route add default via %s dev eth0 2>/dev/null; echo 'nameserver 8.8.8.8' > /etc/resolv.conf", alloc.TAPIP),
		).Run()

		// Apply post-boot config via direct SSH
		applyPostBootDirect(updatedVM, keyPath, ports, netPolicy)

		fmt.Printf("\n  %s is running! (Apple VZ)\n", name)
		fmt.Printf("    IP:   %s\n", alloc.GuestIP)
		printPorts(updatedVM)
		fmt.Printf("    SSH:  mvm ssh %s\n", name)
	} else {
		fmt.Printf("\n  %s started but SSH not ready yet.\n", name)
		fmt.Printf("    SSH:  mvm ssh %s (when ready)\n", name)
	}
	fmt.Println("    Note: pause/resume not available on Apple VZ backend")
	return nil
}

// applyPostBootDirect applies port forwarding and network policy via direct SSH (Apple VZ).
func applyPostBootDirect(vm *state.VM, keyPath string, ports []state.PortMap, netPolicy string) {
	if netPolicy != "" && netPolicy != "open" {
		// Apply iptables rules inside guest via direct SSH
		var rules string
		if netPolicy == "deny" {
			rules = "iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT; iptables -A OUTPUT -p udp --dport 53 -j ACCEPT; iptables -A OUTPUT -o lo -j ACCEPT; iptables -A OUTPUT -j DROP"
		} else if strings.HasPrefix(netPolicy, "allow:") {
			domains := strings.TrimPrefix(netPolicy, "allow:")
			rules = "iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT; iptables -A OUTPUT -p udp --dport 53 -j ACCEPT; iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT; iptables -A OUTPUT -o lo -j ACCEPT"
			for _, domain := range strings.Split(domains, ",") {
				domain = strings.TrimSpace(domain)
				if domain != "" {
					rules += fmt.Sprintf("; for ip in $(getent hosts %s 2>/dev/null | awk '{print $1}'); do iptables -A OUTPUT -d $ip -j ACCEPT; done", domain)
				}
			}
			rules += "; iptables -A OUTPUT -j DROP"
		}
		if rules != "" {
			exec.Command("ssh", "-i", keyPath,
				"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
				fmt.Sprintf("root@%s", vm.GuestIP), rules,
			).Run()
		}
	}
	// Note: port forwarding on Apple VZ uses NAT, not iptables DNAT in Lima.
	// Ports are logged but forwarding depends on VZ network configuration.
}

// execLocal is defined in init.go

