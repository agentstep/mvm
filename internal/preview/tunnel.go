// Package preview exposes a guest TCP port on the host's loopback.
//
// This is the safe, local-only tunnel (à la `kubectl port-forward`): it binds
// 127.0.0.1 and forwards each accepted connection to a guest port over the
// existing agent channel. No public listener, no untrusted content served from
// a host origin — that (the public reverse proxy) is a separate, gated phase.
package preview

import (
	"context"
	"fmt"
	"io"
	"net"
)

// GuestDial opens a raw byte relay to guestPort inside the VM. The VZ
// implementation routes over vsock to the agent's tcp_forward verb; the conn it
// returns is a raw bidirectional stream.
type GuestDial func(ctx context.Context, guestPort int) (net.Conn, error)

// Tunnel forwards a local loopback port to a guest port.
type Tunnel struct {
	GuestPort int
	Dial      GuestDial

	// BindIP is the host address to bind. Empty defaults to "127.0.0.1" —
	// this package's documented safe, local-only default (see the package
	// doc comment). Set explicitly (e.g. "0.0.0.0") to opt into a wider
	// bind; this plan never changes the default itself.
	BindIP string

	ln net.Listener
}

// Listen binds BindIP:localPort (BindIP empty = "127.0.0.1"; localPort 0
// picks a free port) and returns the bound address. It does not accept
// connections until Serve is called.
func (t *Tunnel) Listen(localPort int) (string, error) {
	bindIP := t.BindIP
	if bindIP == "" {
		bindIP = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindIP, localPort))
	if err != nil {
		return "", err
	}
	t.ln = ln
	return ln.Addr().String(), nil
}

// Serve accepts connections until the context is cancelled or the listener is
// closed, relaying each to the guest port. Each connection gets its own guest
// forward, so multiple browser tabs work.
func (t *Tunnel) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		t.ln.Close()
	}()
	for {
		local, err := t.ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go t.handle(ctx, local)
	}
}

func (t *Tunnel) handle(ctx context.Context, local net.Conn) {
	defer local.Close()
	guest, err := t.Dial(ctx, t.GuestPort)
	if err != nil {
		return // guest service not up / refused; drop the connection
	}
	defer guest.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(guest, local); done <- struct{}{} }()
	go func() { io.Copy(local, guest); done <- struct{}{} }()
	<-done
}
