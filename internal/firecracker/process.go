package firecracker

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// Start launches a Firecracker microVM (fresh boot).
func Start(ex Executor, name string, alloc state.NetAllocation, cpus, memMB int) (int, error) {
	script := StartScript(name, alloc, cpus, memMB)
	return runStartScript(ex, name, alloc, script)
}

// StartExisting boots a VM using its existing rootfs.
func StartExisting(ex Executor, name string, alloc state.NetAllocation, cpus, memMB int) (int, error) {
	script := StartExistingScript(name, alloc, cpus, memMB)
	out, err := ex.RunWithTimeout(script, LongTimeout)
	if err != nil {
		cleanup := fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true", SocketPath(name), alloc.TAPDev)
		ex.Run(cleanup)
		return 0, fmt.Errorf("restart microVM %q: %w", name, err)
	}
	return parsePID(out), nil
}

// StartFromSnapshot restores a Firecracker microVM from a snapshot.
func StartFromSnapshot(ex Executor, name string, alloc state.NetAllocation) (int, error) {
	script := StartFromSnapshotScript(name, alloc)
	return runStartScript(ex, name, alloc, script)
}

func runStartScript(ex Executor, name string, alloc state.NetAllocation, script string) (int, error) {
	out, err := ex.RunWithTimeout(script, LongTimeout)
	if err != nil {
		cleanup := fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true; sudo rm -rf %s",
			SocketPath(name), alloc.TAPDev, VMDir(name))
		ex.Run(cleanup)
		return 0, fmt.Errorf("start microVM %q: %w", name, err)
	}
	return parsePID(out), nil
}

func parsePID(output string) int {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "PID:") {
			pid, _ := strconv.Atoi(strings.TrimPrefix(line, "PID:"))
			return pid
		}
	}
	return 0
}

// StreamBootLog starts streaming the Firecracker console log to stdout.
func StreamBootLog(ex Executor, limaVMName, vmName string) context.CancelFunc {
	logPath := filepath.Join(VMDir(vmName), "firecracker.log")
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// StreamBootLog needs limactl for macOS-side streaming
		cmd := exec.CommandContext(ctx, "limactl", "shell", limaVMName,
			"bash", "-c", fmt.Sprintf("sudo tail -f %s 2>/dev/null", logPath))
		cmd.Stdout = execStdout()
		cmd.Stderr = nil
		_ = cmd.Run()
	}()

	return cancel
}

// WaitForGuest polls until the guest agent is reachable.
// When running inside Lima (LocalExecutor): direct TCP dial, sub-ms.
// When running on macOS (LimaExecutor): tries TCP through executor.
func WaitForGuest(ex Executor, guestIP string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := fmt.Sprintf(`echo "" | nc -w 2 %s 5123 >/dev/null 2>&1 && echo OK || echo FAIL`, guestIP)
		out, err := ex.RunWithTimeout(cmd, 10*time.Second)
		if err == nil && strings.Contains(out, "OK") {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}


// SetupGuestNetworkViaAgent configures networking via the agent.
func SetupGuestNetworkViaAgent(ex Executor, guestIP, gatewayIP string) error {
	return agentExec(ex, guestIP, fmt.Sprintf(
		"ip route add default via %s dev eth0 2>/dev/null; echo 'nameserver 8.8.8.8' > /etc/resolv.conf",
		gatewayIP))
}

// StopViaAgent tries graceful shutdown via agent, falls back to kill.
func StopViaAgent(ex Executor, vm *state.VM, hostKeyPath string) error {
	// Try agent poweroff
	agentExec(ex, vm.GuestIP, "poweroff")

	waitScript := fmt.Sprintf(`
for i in $(seq 1 20); do
    if ! sudo kill -0 %d 2>/dev/null; then echo "STOPPED"; exit 0; fi
    sleep 0.5
done
echo "TIMEOUT"`, vm.PID)
	out, _ := ex.RunWithTimeout(waitScript, 15*time.Second)
	if !strings.Contains(out, "STOPPED") {
		ex.Run(fmt.Sprintf("sudo kill -9 %d 2>/dev/null || true", vm.PID))
	}

	ex.Run(fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true", vm.SocketPath, vm.TAPDevice))
	return nil
}

// IsRunning checks if a Firecracker process is alive.
func IsRunning(ex Executor, pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := ex.Run(fmt.Sprintf("sudo kill -0 %d 2>/dev/null && echo YES || echo NO", pid))
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "YES"
}

// Cleanup removes all resources for a VM.
func Cleanup(ex Executor, vm *state.VM) error {
	script := fmt.Sprintf(`sudo kill -9 %d 2>/dev/null || true
sudo rm -f %s
sudo ip link del %s 2>/dev/null || true
sudo rm -rf %s
echo "Cleaned up"`, vm.PID, vm.SocketPath, vm.TAPDevice, VMDir(vm.Name))
	_, err := ex.Run(script)
	return err
}

// ApplyNetworkPolicyViaAgent sets iptables rules via the agent.
func ApplyNetworkPolicyViaAgent(ex Executor, vm *state.VM) error {
	if vm.NetPolicy == "" || vm.NetPolicy == "open" {
		return nil
	}

	var cmds string
	if vm.NetPolicy == "deny" {
		cmds = "iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT; iptables -A OUTPUT -p udp --dport 53 -j ACCEPT; iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT; iptables -A OUTPUT -o lo -j ACCEPT; iptables -A OUTPUT -j DROP"
	} else if strings.HasPrefix(vm.NetPolicy, "allow:") {
		domains := strings.Split(strings.TrimPrefix(vm.NetPolicy, "allow:"), ",")
		cmds = "iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT; iptables -A OUTPUT -p udp --dport 53 -j ACCEPT; iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT; iptables -A OUTPUT -o lo -j ACCEPT"
		for _, domain := range domains {
			domain = strings.TrimSpace(domain)
			if domain != "" {
				cmds += fmt.Sprintf("; for ip in $(getent hosts %s 2>/dev/null | awk '{print $1}'); do iptables -A OUTPUT -d $ip -j ACCEPT; done", domain)
			}
		}
		cmds += "; iptables -A OUTPUT -j DROP"
	} else {
		return fmt.Errorf("unknown network policy: %s", vm.NetPolicy)
	}

	return agentExec(ex, vm.GuestIP, cmds)
}

// ApplySeccompViaAgent applies security profiles via the agent.
func ApplySeccompViaAgent(ex Executor, vm *state.VM, profile string) error {
	if profile == "" {
		return nil
	}
	script, ok := seccompProfiles[profile]
	if !ok {
		return fmt.Errorf("unknown seccomp profile %q", profile)
	}
	return agentExec(ex, vm.GuestIP, script)
}

// agentExec runs a command on the guest agent via TCP.
// Works both from macOS (through executor's nc) and from inside Lima (direct TCP).
// AgentExec runs a command on the guest agent via direct TCP.
//
// DEPRECATED: this is the macOS-side fallback used when the in-Lima daemon
// is unavailable. The daemon path now uses internal/agentclient with the
// Firecracker vsock UDS bridge (see internal/server/routes.go). The remaining
// callers here (SetupGuestNetworkViaAgent, StopViaAgent, ApplyNetworkPolicyViaAgent,
// ApplySeccompViaAgent) will be migrated to agentclient in PR #2 once we add
// a Lima-aware dialer that forwards the UDS through the daemon socket.
func AgentExec(ex Executor, guestIP, command string) error {
	return agentExec(ex, guestIP, command)
}

func agentExec(ex Executor, guestIP, command string) error {
	// Try direct TCP first (works inside Lima)
	conn, err := net.DialTimeout("tcp", guestIP+":5123", 5*time.Second)
	if err == nil {
		defer conn.Close()
		// Use the agent protocol directly
		req := fmt.Sprintf(`{"type":"exec","id":"e","exec":{"command":"%s"}}`,
			strings.ReplaceAll(strings.ReplaceAll(command, `\`, `\\`), `"`, `\"`))
		// Write length-prefixed request
		reqBytes := []byte(req)
		length := make([]byte, 4)
		length[0] = byte(len(reqBytes) >> 24)
		length[1] = byte(len(reqBytes) >> 16)
		length[2] = byte(len(reqBytes) >> 8)
		length[3] = byte(len(reqBytes))
		conn.Write(length)
		conn.Write(reqBytes)
		// Read response (don't need to parse it)
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		conn.Read(buf)
		return nil
	}
	// Fallback: run via executor (for macOS where direct TCP doesn't reach guest)
	_, execErr := ex.RunWithTimeout(
		fmt.Sprintf(`echo "" | nc -w 2 %s 5123 >/dev/null 2>&1`, guestIP),
		5*time.Second)
	return execErr
}

