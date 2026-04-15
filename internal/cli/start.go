package cli

import (
	"context"
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
			portMaps, err := parsePorts(ports)
			if err != nil {
				return err
			}
			return runStart(store, args[0], detach, portMaps, netPolicy, volumes, seccomp, watch, cpus, memoryMB, image)
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
	cmd.Flags().StringVar(&image, "image", "", "custom rootfs image name (built with mvm build)")

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

func runStart(store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, watch string, cpus, memoryMB int, image string) error {
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

	// Firecracker path: route through daemon
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
	})
	if err != nil {
		return err
	}

	fmt.Printf("\n  %s is running!\n", resp.Name)
	fmt.Printf("    IP:   %s\n", resp.GuestIP)
	for _, p := range resp.Ports {
		fmt.Printf("    Port: localhost:%d -> %s:%d/%s\n", p.HostPort, resp.GuestIP, p.GuestPort, p.Proto)
	}
	fmt.Printf("    Exec: mvm exec %s -- <command>\n", resp.Name)

	return nil
}

func printPorts(vm *state.VM) {
	for _, p := range vm.Ports {
		fmt.Printf("    Port: localhost:%d -> %s:%d/%s\n", p.HostPort, vm.GuestIP, p.GuestPort, p.Proto)
	}
}

// runStartAppleVZ starts a VM using the Apple Virtualization.framework backend.
//
// As of PR #2 this path drives the in-guest agent over vsock via the
// per-VM mvm-vz helper IPC socket — no SSH, no TAP-IP TCP, no Lima.
// The previous SSH-based post-boot path (and the applyPostBootDirect
// helper that went with it) has been removed.
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

	fmt.Printf("Starting microVM '%s' (Apple VZ)...\n", name)

	vzCpus := cpus
	if vzCpus <= 0 {
		vzCpus = 2
	}
	vzMem := memoryMB
	if vzMem <= 0 {
		vzMem = 1024
	}
	startResult, err := vzBackend.StartVM(name, kernelPath, vmRootfs, bootArgs, alloc.GuestMAC, vzCpus, vzMem, volumes)
	if err != nil {
		store.RemoveVM(name)
		return fmt.Errorf("start VM: %w", err)
	}
	pid := startResult.PID

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
		Name:       name,
		Status:     "running",
		GuestIP:    alloc.GuestIP,
		TAPIP:      alloc.TAPIP,
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
	fmt.Println("  Waiting for guest agent...")
	agentReady := waitForAgent(agent, 60*time.Second)

	if agentReady {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Configure guest networking via the agent.
		setupCmd := fmt.Sprintf(
			"ip route add default via %s dev eth0 2>/dev/null; echo 'nameserver 8.8.8.8' > /etc/resolv.conf",
			alloc.TAPIP,
		)
		if _, err := agent.Exec(ctx, setupCmd, ""); err != nil {
			fmt.Printf("  Warning: setup network: %v\n", err)
		}

		// Apply network policy via the agent.
		if err := applyVZNetworkPolicy(ctx, agent, netPolicy); err != nil {
			fmt.Printf("  Warning: apply network policy: %v\n", err)
		}

		fmt.Printf("\n  %s is running! (Apple VZ)\n", name)
		fmt.Printf("    IP:   %s\n", alloc.GuestIP)
		printPorts(updatedVM)
		fmt.Printf("    Exec: mvm exec %s -- <command>\n", name)
	} else {
		fmt.Printf("\n  %s started but agent not reachable yet.\n", name)
		fmt.Printf("    Exec: mvm exec %s -- <command>  (when ready)\n", name)
	}
	return nil
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
