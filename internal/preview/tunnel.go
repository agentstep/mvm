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

	ln net.Listener
}

// Listen binds 127.0.0.1:localPort (localPort 0 picks a free port) and returns
// the bound address. It does not accept connections until Serve is called.
func (t *Tunnel) Listen(localPort int) (string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
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
