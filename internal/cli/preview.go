package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentstep/mvm/internal/preview"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

// agentPort is the guest control port; never previewable.
const agentPort = 5123

func newPreviewCmd(store *state.Store) *cobra.Command {
	var localPort int
	cmd := &cobra.Command{
		Use:   "preview <vm> <guest-port>",
		Short: "Forward a guest port to a local loopback port (applevz)",
		Long: `Open a private tunnel from your machine to a port inside a sandbox, like
kubectl port-forward. Binds 127.0.0.1 only — no public URL.

The guest port must have been published at start (mvm start <vm> -p <port>:<port>);
previewing an undeclared port is refused.

  mvm start web -p 3000:3000
  mvm preview web 3000            # -> http://127.0.0.1:3000
  mvm preview web 3000 --local-port 8080`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			guestPort, err := net.LookupPort("tcp", args[1])
			if err != nil {
				return fmt.Errorf("invalid guest port %q", args[1])
			}
			return runPreview(store, args[0], guestPort, localPort)
		},
	}
	cmd.Flags().IntVar(&localPort, "local-port", 0, "local port to bind (default: same as guest port, else a free port)")
	return cmd
}

func runPreview(store *state.Store, name string, guestPort, localPort int) error {
	vm, err := store.GetVM(name)
	if err != nil || vm == nil {
		return fmt.Errorf("microVM %q not found", name)
	}
	if vm.Backend != "applevz" {
		return fmt.Errorf("mvm preview currently supports the applevz backend only")
	}
	if vm.Status != "running" && vm.Status != "paused" {
		return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
	}
	// Deny-by-default: only forward a port the VM actually published, and never
	// the agent's control port.
	if guestPort == agentPort {
		return fmt.Errorf("port %d is the guest control port and cannot be previewed", guestPort)
	}
	if !portPublished(vm, guestPort) {
		return fmt.Errorf("port %d was not published for %q; start it with: mvm start %s -p %d:%d", guestPort, name, name, guestPort, guestPort)
	}

	if localPort == 0 {
		localPort = guestPort
	}
	agent := vm_pkg.NewAppleVZBackend(mvmDir).AgentClient(name)
	tun := &preview.Tunnel{
		GuestPort: guestPort,
		Dial: func(ctx context.Context, port int) (net.Conn, error) {
			return agent.Forward(ctx, port)
		},
	}
	addr, err := tun.Listen(localPort)
	if err != nil {
		// Fall back to a free port if the requested one is taken.
		addr, err = tun.Listen(0)
		if err != nil {
			return fmt.Errorf("bind local port: %w", err)
		}
	}

	fmt.Printf("  Forwarding http://%s -> %s guest port %d\n", addr, name, guestPort)
	fmt.Println("  Press Ctrl-C to stop.")

	ctx, cancel := signalContext()
	defer cancel()
	if err := tun.Serve(ctx); err != nil {
		return err
	}
	fmt.Println("\n  Tunnel closed.")
	return nil
}

func portPublished(vm *state.VM, guestPort int) bool {
	for _, p := range vm.Ports {
		if p.GuestPort == guestPort {
			return true
		}
	}
	return false
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
