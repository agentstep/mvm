package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agentstep/mvm/internal/preview"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

// forwardDaemonPortResult reports the outcome of binding one published port.
type forwardDaemonPortResult struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Bound     bool   `json:"bound"`
	Error     string `json:"error,omitempty"`
}

// forwardDaemonStatus is the single JSON status line this command prints to
// stdout once it has attempted every published port — mirrors the mvm-vz
// helper's own ready-line convention (see AppleVZBackend.StartVM), so the
// parent (runStartAppleVZ) can synchronize on it the same way.
type forwardDaemonStatus struct {
	PID   int                       `json:"pid"`
	Ports []forwardDaemonPortResult `json:"ports"`
}

// newForwardDaemonCmd is an internal, hidden command: it holds the host-side
// listeners for `mvm start -p host:guest` on the applevz backend and relays
// each accepted connection to the guest over the existing vsock tcp_forward
// channel (see internal/preview.Tunnel and agentclient.Client.Forward — the
// same primitives that already power `mvm preview`).
//
// runStartAppleVZ spawns this as a detached child (setsid, own session) so
// the forwarders outlive the `mvm start` CLI invocation itself, exactly like
// the mvm-vz helper does for the VM. Its PID is recorded in state
// (VM.ForwarderPID) so `mvm stop` can tear it down — see killForwarder in
// stop.go. Nothing about a guest's own network route (Bug 1) matters here:
// the vsock forward path never touches the guest's IP stack at all.
func newForwardDaemonCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:    "__forward-ports <name>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runForwardDaemon(store, args[0])
		},
	}
}

// portForwardProtoSupported reports whether the applevz forwarder can carry
// a PortMap's protocol. Only TCP is supported: the underlying transport is
// the agent's tcp_forward vsock relay (a byte stream), which has no UDP
// datagram equivalent. An empty proto defaults to tcp (see parsePorts).
func portForwardProtoSupported(proto string) bool {
	return proto == "" || proto == "tcp"
}

func runForwardDaemon(store *state.Store, name string) error {
	vm, err := store.GetVM(name)
	if err != nil || vm == nil {
		return fmt.Errorf("microVM %q not found", name)
	}

	agent := vm_pkg.NewAppleVZBackend(mvmDir).AgentClient(name)

	status := forwardDaemonStatus{PID: os.Getpid()}
	var tunnels []*preview.Tunnel
	for _, p := range vm.Ports {
		res := forwardDaemonPortResult{HostPort: p.HostPort, GuestPort: p.GuestPort}
		if !portForwardProtoSupported(p.Proto) {
			res.Error = fmt.Sprintf("proto %q not supported for applevz port forwarding (tcp only)", p.Proto)
			status.Ports = append(status.Ports, res)
			continue
		}

		guestPort := p.GuestPort
		tun := &preview.Tunnel{
			GuestPort: guestPort,
			Dial: func(ctx context.Context, port int) (net.Conn, error) {
				return agent.Forward(ctx, port)
			},
		}
		if _, err := tun.Listen(p.HostPort); err != nil {
			res.Error = err.Error()
			status.Ports = append(status.Ports, res)
			continue
		}
		res.Bound = true
		status.Ports = append(status.Ports, res)
		tunnels = append(tunnels, tun)
	}

	line, err := json.Marshal(status)
	if err != nil {
		return err
	}
	fmt.Println(string(line))
	_ = os.Stdout.Sync()

	if len(tunnels) == 0 {
		// Nothing bound — nothing to serve, and nothing to leave running.
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	done := make(chan struct{}, len(tunnels))
	for _, tun := range tunnels {
		go func(t *preview.Tunnel) {
			_ = t.Serve(ctx) // returns nil on ctx cancellation, an error only if Accept itself failed
			done <- struct{}{}
		}(tun)
	}
	for range tunnels {
		<-done
	}
	return nil
}

// spawnPortForwarders launches the hidden __forward-ports command as a
// detached child (its own session, so it survives the parent `mvm start`
// process exiting) and waits for its one-line ready report. It returns the
// forwarder's PID (recorded into state so `mvm stop` can kill it) and an
// error describing any ports that failed to bind — the caller decides
// whether that's fatal.
//
// Deliberately mirrors AppleVZBackend.StartVM's own status-line convention
// AND its documented pipe-hygiene fix: the child's stdout is a pipe we
// actively drain, and stderr is /dev/null (cmd.Stderr left nil) rather than
// inherited — an inherited fd on a long-lived detached child is exactly the
// stdout/stderr pipe-hang class of bug documented for mvm-vz itself; new
// code here shouldn't reintroduce it.
func spawnPortForwarders(name string) (pid int, err error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate mvm binary: %w", err)
	}

	cmd := exec.Command(self, "__forward-ports", name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start port forwarder: %w", err)
	}

	br := bufio.NewReader(stdoutPipe)
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := br.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- strings.TrimSpace(line)
	}()

	var jsonLine string
	select {
	case jsonLine = <-lineCh:
	case err := <-errCh:
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, fmt.Errorf("read port forwarder status: %w", err)
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, fmt.Errorf("timeout waiting for port forwarder to report ready")
	}
	// Drain the rest of stdout in the background so the child never blocks
	// on a full pipe for the rest of its (long) life.
	go func() { _, _ = io.Copy(io.Discard, br) }()

	var status forwardDaemonStatus
	if err := json.Unmarshal([]byte(jsonLine), &status); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, fmt.Errorf("parse port forwarder status %q: %w", jsonLine, err)
	}

	var failures []string
	for _, p := range status.Ports {
		if !p.Bound {
			failures = append(failures, fmt.Sprintf("%d->%d: %s", p.HostPort, p.GuestPort, p.Error))
		}
	}
	if len(failures) > 0 {
		return status.PID, fmt.Errorf("some ports failed to bind: %s", strings.Join(failures, "; "))
	}
	return status.PID, nil
}

// killForwarder terminates a VM's detached port-forwarder process, if any.
// Safe to call when ForwarderPID is 0 (no forwarders were ever started) or
// already dead (stale PID from a crash) — always clears the field.
func killForwarder(store *state.Store, name string, forwarderPID int) {
	if forwarderPID <= 0 {
		return
	}
	proc, err := os.FindProcess(forwarderPID)
	if err == nil {
		_ = proc.Signal(syscall.SIGTERM)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if proc.Signal(syscall.Signal(0)) != nil {
				break // exited
			}
			time.Sleep(50 * time.Millisecond)
		}
		if proc.Signal(syscall.Signal(0)) == nil {
			_ = proc.Kill() // still alive after the grace period — force it
		}
	}
	_ = store.UpdateVM(name, func(v *state.VM) { v.ForwarderPID = 0 })
}
