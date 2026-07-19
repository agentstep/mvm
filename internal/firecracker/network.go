package firecracker

import (
	"fmt"
	"strings"

	"github.com/agentstep/mvm/internal/state"
)

// SetupPortForwarding creates iptables DNAT rules in Lima to forward
// host ports to guest ports inside the microVM. A non-empty p.HostIP adds a
// destination-address filter (-d) to each rule, restricting it to traffic
// addressed to that IP inside Lima's own network namespace; empty (the
// default) matches any destination, unchanged from prior behavior.
func SetupPortForwarding(exec Executor, vm *state.VM) error {
	if len(vm.Ports) == 0 {
		return nil
	}

	var rules []string
	for _, p := range vm.Ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		destFilter := ""
		if p.HostIP != "" {
			destFilter = fmt.Sprintf(" -d %s", p.HostIP)
		}
		// DNAT: traffic to Lima's localhost:hostPort -> guest:guestPort
		rules = append(rules,
			fmt.Sprintf("sudo iptables -t nat -A PREROUTING -p %s%s --dport %d -j DNAT --to-destination %s:%d",
				proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort),
			// Also handle locally-originated traffic
			fmt.Sprintf("sudo iptables -t nat -A OUTPUT -p %s%s --dport %d -j DNAT --to-destination %s:%d",
				proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort),
		)
	}

	cmd := strings.Join(rules, " && ")
	_, err := exec.Run(cmd)
	if err != nil {
		return fmt.Errorf("setup port forwarding: %w", err)
	}
	return nil
}

// RemovePortForwarding removes iptables DNAT rules for a VM. The -d filter
// (if any) must match exactly what SetupPortForwarding added, or the
// deletion silently won't find the rule.
func RemovePortForwarding(exec Executor, vm *state.VM) {
	for _, p := range vm.Ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		destFilter := ""
		if p.HostIP != "" {
			destFilter = fmt.Sprintf(" -d %s", p.HostIP)
		}
		exec.Run(fmt.Sprintf(
			"sudo iptables -t nat -D PREROUTING -p %s%s --dport %d -j DNAT --to-destination %s:%d 2>/dev/null; "+
				"sudo iptables -t nat -D OUTPUT -p %s%s --dport %d -j DNAT --to-destination %s:%d 2>/dev/null",
			proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort,
			proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort,
		))
	}
}

// Pause freezes a running VM via the Firecracker API.
func Pause(exec Executor, vm *state.VM) error {
	cmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PATCH "http://localhost/vm" -H "Content-Type: application/json" -d '{"state": "Paused"}'`,
		vm.SocketPath,
	)
	out, err := exec.Run(cmd)
	if err != nil {
		return fmt.Errorf("pause VM: %w", err)
	}
	if strings.Contains(out, "error") {
		return fmt.Errorf("pause VM: %s", out)
	}
	return nil
}

// Resume unfreezes a paused VM via the Firecracker API.
func Resume(exec Executor, vm *state.VM) error {
	cmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PATCH "http://localhost/vm" -H "Content-Type: application/json" -d '{"state": "Resumed"}'`,
		vm.SocketPath,
	)
	out, err := exec.Run(cmd)
	if err != nil {
		return fmt.Errorf("resume VM: %w", err)
	}
	if strings.Contains(out, "error") {
		return fmt.Errorf("resume VM: %s", out)
	}
	return nil
}
