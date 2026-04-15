package agentclient

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// Dialer opens a single bidirectional connection to the in-guest mvm-agent.
//
// Implementations:
//
//   - FirecrackerVsockDialer: dials Firecracker's vsock-over-Unix-socket
//     bridge with the CONNECT handshake. Used by the daemon inside Lima.
//
// Future:
//
//   - VZSocketDialer: receives a vsock fd over SCM_RIGHTS from the long-running
//     mvm-vz helper. (Stage 1 of the VZ co-equal plan.)
//
//   - TCPDialer: legacy TCP-via-TAP fallback. Kept until Stage 2 ships
//     and we cut the macOS-side fallback path.
type Dialer interface {
	// Dial returns a fresh connection to the agent. The connection has no
	// deadline set; the Client sets request-scoped deadlines from ctx.
	Dial(ctx context.Context) (net.Conn, error)

	// String identifies this dialer in error messages.
	String() string
}

// FirecrackerVsockDialer dials the agent through a Firecracker microVM's
// vsock-to-Unix-socket bridge.
//
// Firecracker exposes the guest vsock as:
//
//	{UDSPath}                — the base socket. Host→guest connections are
//	                           established by writing "CONNECT <port>\n" and
//	                           reading "OK <hostcid>\n" before the bidirectional
//	                           byte stream begins.
//	{UDSPath}_<port>         — guest→host connections, one socket per port the
//	                           guest contacts. Not used by this dialer.
type FirecrackerVsockDialer struct {
	// UDSPath is the path to Firecracker's vsock UDS, e.g.
	// "/run/mvm/foo.vsock".
	UDSPath string

	// GuestPort is the vsock port the agent listens on. Defaults to 5123.
	GuestPort uint32

	// HandshakeTimeout bounds the CONNECT/OK exchange. Defaults to 5s.
	HandshakeTimeout time.Duration
}

// Dial connects to the Firecracker vsock UDS and performs the CONNECT handshake.
//
// On success, the returned net.Conn is positioned at the start of the
// bidirectional byte stream to the guest agent. The deadline is cleared
// before return; the Client sets per-request deadlines from its own context.
func (d *FirecrackerVsockDialer) Dial(ctx context.Context) (net.Conn, error) {
	port := d.GuestPort
	if port == 0 {
		port = 5123
	}
	timeout := d.HandshakeTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", d.UDSPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", d.UDSPath, err)
	}

	// Bound the handshake. The caller's context may already have a deadline;
	// take whichever is sooner.
	hsDeadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(hsDeadline) {
		hsDeadline = dl
	}
	if err := conn.SetDeadline(hsDeadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}

	line, err := readLine(conn, 64)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read OK: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("firecracker rejected CONNECT %d on %s: %q", port, d.UDSPath, line)
	}

	// Clear the handshake deadline. The Client will set its own per-request
	// deadline from its context before the first read/write.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}

	return conn, nil
}

// String returns a human-readable identifier for error messages.
func (d *FirecrackerVsockDialer) String() string {
	port := d.GuestPort
	if port == 0 {
		port = 5123
	}
	return fmt.Sprintf("firecracker-vsock(%s, port=%d)", d.UDSPath, port)
}

// readLine reads bytes one at a time until a newline or maxLen is hit.
//
// We avoid bufio.Reader here because the connection is handed back to the
// caller after the handshake; any bytes buffered past the newline would be
// silently lost. The handshake is a single short line (~16 bytes), so the
// per-byte read cost is irrelevant.
func readLine(r net.Conn, maxLen int) (string, error) {
	var buf [1]byte
	out := make([]byte, 0, maxLen)
	for len(out) < maxLen {
		n, err := r.Read(buf[:])
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			return strings.TrimRight(string(out), "\r"), nil
		}
		out = append(out, buf[0])
	}
	return "", fmt.Errorf("line exceeded %d bytes without newline", maxLen)
}
