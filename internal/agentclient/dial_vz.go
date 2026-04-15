package agentclient

import (
	"context"
	"fmt"
	"net"

	"github.com/agentstep/mvm/internal/vzhelper"
)

// VZSocketDialer dials the in-guest mvm-agent through the per-VM
// mvm-vz Swift helper.
//
// Mechanism:
//
//  1. Connect to the helper's Unix socket at SocketPath.
//  2. Send a "connect" command with the agent's vsock guest port.
//  3. The helper opens a VZVirtioSocketConnection inside the Apple
//     Virtualization framework, then passes the resulting file descriptor
//     back via SCM_RIGHTS in a single recvmsg call.
//  4. The fd is wrapped as a *net.UnixConn-equivalent via os.NewFile +
//     net.FileConn, and returned to the caller.
//
// The wrapped connection looks and behaves like any other net.Conn: the
// Client can speak the agent's length-prefixed JSON protocol over it
// without knowing or caring that the underlying transport is a vsock fd
// dup'd in from another process.
type VZSocketDialer struct {
	// SocketPath is the path to the per-VM mvm-vz helper IPC socket,
	// typically ~/.mvm/run/vz-<name>.sock.
	SocketPath string

	// GuestPort is the vsock port the agent listens on. Defaults to 5123.
	GuestPort uint32
}

// Dial asks the mvm-vz helper to open a vsock connection to the agent
// and returns the resulting fd as a net.Conn.
func (d *VZSocketDialer) Dial(ctx context.Context) (net.Conn, error) {
	port := d.GuestPort
	if port == 0 {
		port = 5123
	}

	helper := vzhelper.New(d.SocketPath)
	f, err := helper.Connect(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("vz helper connect: %w", err)
	}

	// Wrap the raw fd as a net.Conn. net.FileConn duplicates the fd
	// internally (via dup) and closes the original *os.File. The returned
	// conn has its own fd; closing it closes that fd. The kernel
	// ref-counts the underlying socket file, so the Apple-VF side stays
	// alive as long as the helper holds its connection object.
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("wrap fd as net.Conn: %w", err)
	}
	return conn, nil
}

// String returns a human-readable identifier for error messages.
func (d *VZSocketDialer) String() string {
	port := d.GuestPort
	if port == 0 {
		port = 5123
	}
	return fmt.Sprintf("vz-helper(%s, port=%d)", d.SocketPath, port)
}
