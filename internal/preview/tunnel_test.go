package preview

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// echoServer stands in for a guest service: it echoes lines back.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln
}

func TestTunnelRelays(t *testing.T) {
	guest := echoServer(t)
	defer guest.Close()

	tun := &Tunnel{
		GuestPort: 1234, // ignored by this dialer
		Dial: func(ctx context.Context, port int) (net.Conn, error) {
			return net.Dial("tcp", guest.Addr().String())
		},
	}
	addr, err := tun.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Serve(ctx)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	fmt.Fprintf(conn, "hello\n")
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if got != "hello\n" {
		t.Fatalf("echo = %q, want %q", got, "hello\n")
	}
}

// A dial failure (guest service down) must close the local conn cleanly,
// not hang.
func TestTunnelDialFailureClosesConn(t *testing.T) {
	tun := &Tunnel{
		GuestPort: 1234,
		Dial: func(ctx context.Context, port int) (net.Conn, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	addr, err := tun.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Serve(ctx)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	// The relay never starts; the server closes our conn, so a read returns
	// EOF promptly rather than hanging.
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != io.EOF {
		t.Fatalf("read = %v, want EOF after dial failure", err)
	}
}
