package agentclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// DefaultRequestTimeout is applied to a request if the caller's ctx has no
// deadline. Long enough for Node.js-based agents like Claude Code to start up
// inside a guest, short enough to fail fast on a wedged VM.
const DefaultRequestTimeout = 5 * time.Minute

// Client is a stateless host-side client for the in-guest mvm-agent.
//
// Each method opens a fresh connection via the Dialer, sends one request,
// reads the response, and closes. There is no connection pooling — the
// underlying transport (Firecracker vsock UDS) is sub-millisecond to dial,
// so pooling would add complexity for no measurable gain.
//
// A Client is safe for concurrent use; the underlying Dialer must also be
// safe for concurrent use (FirecrackerVsockDialer is).
type Client struct {
	dialer Dialer
}

// New returns a Client that dials via d.
func New(d Dialer) *Client {
	return &Client{dialer: d}
}

// ExecResult is the result of running a command on the guest.
type ExecResult struct {
	// Output is the combined stdout+stderr from the command, in the order
	// the agent buffered it. Matches the existing daemon behavior.
	Output string

	// ExitCode is the command's exit status.
	ExitCode int
}

// Ping verifies the agent is reachable and responsive.
func (c *Client) Ping(ctx context.Context) error {
	req := &request{Type: reqPing, ID: newID()}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return err
	}
	if resp.Type == respError {
		return fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respOK {
		return fmt.Errorf("unexpected ping response type: %q", resp.Type)
	}
	return nil
}

// Exec runs a shell command on the guest and returns its combined output
// and exit code. stdin may be empty.
//
// The agent runs the command via "sh -c", so shell metacharacters in command
// are interpreted by the guest shell — this matches the existing daemon
// behavior in execOnGuest.
func (c *Client) Exec(ctx context.Context, command, stdin string) (*ExecResult, error) {
	req := &request{
		Type: reqExec,
		ID:   newID(),
		Exec: &execPayload{Command: command, Stdin: stdin},
	}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return nil, err
	}
	if resp.Type == respError {
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respExit {
		return nil, fmt.Errorf("unexpected exec response type: %q", resp.Type)
	}
	return &ExecResult{
		Output:   string(resp.Data),
		ExitCode: resp.ExitCode,
	}, nil
}

// ExecInteractive runs command in a PTY on the guest and relays the local
// terminal to it until the command exits, returning the command's exit code.
//
// The caller owns terminal mode: put stdin into raw mode before calling and
// restore it after. stdin/stdout are the local terminal ends. resize (may be
// nil) delivers {rows, cols} updates, e.g. from a SIGWINCH handler.
//
// Unlike the other methods this holds one connection open for the whole
// session (the agent's exec_pty takes over the connection).
func (c *Client) ExecInteractive(ctx context.Context, command string, rows, cols uint16, termType string, env map[string]string, stdin io.Reader, stdout io.Writer, resize <-chan [2]uint16) (int, error) {
	conn, err := c.dialer.Dial(ctx)
	if err != nil {
		return -1, fmt.Errorf("dial agent: %w", err)
	}
	defer conn.Close()

	// Start the PTY session and wait for the agent's initial OK.
	if err := writeFrame(conn, &request{
		Type: reqExecPty,
		ID:   newID(),
		Pty:  &ptyPayload{Command: command, Env: env, Rows: rows, Cols: cols, Term: termType},
	}); err != nil {
		return -1, fmt.Errorf("send exec_pty: %w", err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		return -1, fmt.Errorf("read agent response: %w", err)
	}
	if resp.Type == respError {
		return -1, fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respOK {
		return -1, fmt.Errorf("unexpected agent response %q", resp.Type)
	}

	exitCode := -1
	var wg sync.WaitGroup

	// Agent -> local stdout, until the exit frame. This is the goroutine we
	// wait on; the others are fire-and-forget and unblock when conn closes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			var f response
			if err := readFrame(conn, &f); err != nil {
				return
			}
			switch f.Type {
			case respStdout:
				if len(f.Data) > 0 {
					stdout.Write(f.Data)
				}
			case respExit:
				exitCode = f.ExitCode
				return
			}
		}
	}()

	// Local stdin -> agent.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := stdin.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				if werr := writeFrame(conn, &response{Type: respStdin, Data: data}); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Terminal resize -> agent. The agent decodes exit_code as rows<<16|cols.
	if resize != nil {
		go func() {
			for sz := range resize {
				if werr := writeFrame(conn, &response{Type: respResize, ExitCode: int(sz[0])<<16 | int(sz[1])}); werr != nil {
					return
				}
			}
		}()
	}

	wg.Wait()
	return exitCode, nil
}

// Forward opens a raw byte relay to a TCP port on the guest's loopback. It
// dials the agent, sends a tcp_forward request, waits for the OK frame, then
// returns the live connection — after which the stream is raw in both
// directions. The caller owns the returned conn and must Close it; piping an
// inbound tunnel connection through it both ways completes the forward.
func (c *Client) Forward(ctx context.Context, guestPort int) (net.Conn, error) {
	conn, err := c.dialer.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial agent: %w", err)
	}
	if err := writeFrame(conn, &request{
		Type:    reqTCPForward,
		ID:      newID(),
		Forward: &forwardPayload{Port: guestPort},
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send tcp_forward: %w", err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read agent response: %w", err)
	}
	if resp.Type == respError {
		conn.Close()
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respOK {
		conn.Close()
		return nil, fmt.Errorf("unexpected agent response %q", resp.Type)
	}
	return conn, nil
}

// Poweroff requests a graceful guest shutdown.
//
// The agent writes a final response and then powers off, so the connection
// may be torn down before the response arrives. Both outcomes are treated
// as success.
func (c *Client) Poweroff(ctx context.Context) error {
	req := &request{Type: reqPoweroff, ID: newID()}
	var resp response
	err := c.exchange(ctx, req, &resp)
	if err == nil {
		return nil
	}
	// If the read failed because the guest tore down the link, that's the
	// expected outcome of a successful poweroff. Only swallow errors that
	// indicate a closed connection — not timeouts, which mean the guest
	// didn't respond.
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// exchange opens a connection, writes one request, optionally reads one
// response, and closes the connection.
func (c *Client) exchange(ctx context.Context, req *request, resp *response) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultRequestTimeout)
		defer cancel()
	}

	conn, err := c.dialer.Dial(ctx)
	if err != nil {
		return fmt.Errorf("dial agent (%s): %w", c.dialer, err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return fmt.Errorf("set deadline: %w", err)
		}
	}

	if err := writeFrame(conn, req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	if resp == nil {
		return nil
	}
	if err := readFrame(conn, resp); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	return nil
}

// newID returns a short hex token for correlating requests with responses.
// Frames are exchanged on dedicated connections so collisions don't matter,
// but the agent echoes the ID back and including a unique value makes
// trace-based debugging far easier.
func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
