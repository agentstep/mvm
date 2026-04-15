package vzhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// Client talks to a single per-VM mvm-vz Swift helper over its IPC socket.
//
// A Client serializes calls internally via a mutex and is safe for
// concurrent use from multiple goroutines. Concurrent callers will
// block each other since each call opens a fresh connection and holds
// it for the entire request/response exchange.
type Client struct {
	socketPath string

	// dialTimeout bounds the initial Unix-socket connect.
	dialTimeout time.Duration

	// mu serializes calls so concurrent callers don't interleave on
	// the same connection.
	mu sync.Mutex
}

// New returns a Client targeting the helper IPC socket at path.
func New(socketPath string) *Client {
	return &Client{
		socketPath:  socketPath,
		dialTimeout: 5 * time.Second,
	}
}

// Status queries the helper for the VM's current state ("running", "paused", ...).
func (c *Client) Status(ctx context.Context) (string, error) {
	resp, err := c.exchange(ctx, &Request{Cmd: CmdStatus})
	if err != nil {
		return "", err
	}
	return resp.State, nil
}

// Pause asks the helper to pause the VM. On VZ this is a memory-resident
// pause (vCPUs frozen, RAM unchanged); resume is essentially instant.
func (c *Client) Pause(ctx context.Context) error {
	_, err := c.exchange(ctx, &Request{Cmd: CmdPause})
	return err
}

// Resume reverses a previous Pause.
func (c *Client) Resume(ctx context.Context) error {
	_, err := c.exchange(ctx, &Request{Cmd: CmdResume})
	return err
}

// Stop asks the helper to gracefully shut the VM down. The helper
// process exits shortly afterward, and the IPC socket is removed.
//
// A successful Stop tears the helper down, which usually also tears the
// connection down before/while we read the response. We treat any of
// {EOF, EPIPE, ECONNRESET, generic net.Error} as a successful tear-down.
func (c *Client) Stop(ctx context.Context) error {
	_, err := c.exchange(ctx, &Request{Cmd: CmdStop})
	if err == nil {
		return nil
	}
	if isConnTeardown(err) {
		return nil
	}
	return err
}

// isConnTeardown reports whether err looks like the helper closed the
// connection out from under us — which is the expected outcome of a
// successful Stop. Only matches errors that indicate a closed connection,
// not timeouts (which would mean the helper is hung, not torn down).
func isConnTeardown(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return false
}

// Connect asks the helper to open a vsock connection from the host to the
// in-guest agent on guestPort, and returns the resulting file descriptor
// duplicated into this process via SCM_RIGHTS.
//
// The returned *os.File owns the fd; the caller should net.FileConn(f)
// to wrap it as a net.Conn and Close it when done. Closing the file does
// NOT affect the corresponding Apple-VF connection in the helper process —
// the helper holds its own reference to keep the underlying file alive.
func (c *Client) Connect(ctx context.Context, guestPort uint32) (*os.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := c.applyDeadline(ctx, conn); err != nil {
		return nil, err
	}

	if err := writeFrame(conn, &Request{Cmd: CmdConnect, Port: guestPort}); err != nil {
		return nil, fmt.Errorf("write connect request: %w", err)
	}

	// The helper's response is a length-prefixed JSON frame delivered in a
	// single sendmsg() with SCM_RIGHTS ancillary on the first byte. We must
	// receive it in a single recvmsg() — splitting reads loses the ancillary.
	const oobSize = 64 // CMSG_SPACE(sizeof(int)) is well under this on darwin
	const dataSize = 4096
	dataBuf := make([]byte, dataSize)
	oobBuf := make([]byte, oobSize)

	n, oobn, _, _, err := conn.ReadMsgUnix(dataBuf, oobBuf)
	if err != nil {
		return nil, fmt.Errorf("recvmsg: %w", err)
	}

	resp, err := decodeFrameFromBuffer(dataBuf[:n])
	if err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("helper rejected connect: %s", resp.Error)
	}
	if oobn == 0 {
		return nil, fmt.Errorf("helper returned ok with no fd: %+v", resp)
	}

	fd, err := extractFd(oobBuf[:oobn])
	if err != nil {
		return nil, fmt.Errorf("extract fd: %w", err)
	}

	// Mark the fd close-on-exec so children of the Go process don't inherit
	// the live agent connection. This is best-effort — older Linux/macOS
	// kernels don't always honor it on fds received over SCM_RIGHTS, but
	// the call is harmless if it fails.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 {
		// Don't fail the call for this — the fd is still valid.
	}

	name := fmt.Sprintf("vz-vsock-%s-port-%d", c.socketPath, guestPort)
	return os.NewFile(uintptr(fd), name), nil
}

// dial opens a fresh connection to the helper IPC socket.
func (c *Client) dial(ctx context.Context) (*net.UnixConn, error) {
	dialCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, c.dialTimeout)
		defer cancel()
	}

	var d net.Dialer
	raw, err := d.DialContext(dialCtx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial helper %s: %w", c.socketPath, err)
	}
	uc, ok := raw.(*net.UnixConn)
	if !ok {
		raw.Close()
		return nil, fmt.Errorf("expected *net.UnixConn, got %T", raw)
	}
	return uc, nil
}

// exchange writes one request, reads one response (no SCM_RIGHTS), and
// returns the parsed Response. Used for all commands except Connect.
func (c *Client) exchange(ctx context.Context, req *Request) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := c.applyDeadline(ctx, conn); err != nil {
		return nil, err
	}

	if err := writeFrame(conn, req); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	var resp Response
	if err := readFrame(conn, &resp); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("helper error: %s", resp.Error)
	}
	return &resp, nil
}

func (c *Client) applyDeadline(ctx context.Context, conn net.Conn) error {
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return fmt.Errorf("set deadline: %w", err)
		}
	}
	return nil
}

// extractFd parses one fd out of the SCM_RIGHTS ancillary data returned
// by recvmsg. Returns an error if no fd is present or multiple fds are
// present (we only ever pass one).
func extractFd(oob []byte) (int, error) {
	cms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, fmt.Errorf("parse cmsg: %w", err)
	}
	for _, cm := range cms {
		if cm.Header.Level != syscall.SOL_SOCKET || cm.Header.Type != syscall.SCM_RIGHTS {
			continue
		}
		fds, err := syscall.ParseUnixRights(&cm)
		if err != nil {
			return -1, fmt.Errorf("parse unix rights: %w", err)
		}
		if len(fds) == 0 {
			continue
		}
		// Close any extras — we only expect one.
		for _, extra := range fds[1:] {
			syscall.Close(extra)
		}
		return fds[0], nil
	}
	return -1, fmt.Errorf("no SCM_RIGHTS fd in ancillary data")
}

// decodeFrameFromBuffer parses a length-prefixed JSON frame from a byte
// slice that contains exactly one frame (as returned by ReadMsgUnix on
// a stream socket where the sender wrote one full frame in one sendmsg).
func decodeFrameFromBuffer(buf []byte) (*Response, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("short frame: %d bytes", len(buf))
	}
	size := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	if int(4+size) > len(buf) {
		return nil, fmt.Errorf("truncated frame: have %d bytes, header says %d", len(buf)-4, size)
	}
	body := buf[4 : 4+size]
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
