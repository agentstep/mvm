package agentclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// NetInfo asks the guest to self-report its current network configuration
// (its eth0 IPv4 address and default-route gateway), discovered in-guest via
// the Go net package and /proc/net/route — no ip/ifconfig binary required.
//
// This is the applevz backend's answer to "what address did DHCP actually
// hand out": the host has no other way to learn it, since Apple's
// VZNATNetworkDeviceAttachment manages its own DHCP pool transparently (see
// internal/cli/start.go's post-boot discovery step).
func (c *Client) NetInfo(ctx context.Context) (*NetInfo, error) {
	req := &request{Type: reqNetInfo, ID: newID()}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return nil, err
	}
	if resp.Type == respError {
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respOK {
		return nil, fmt.Errorf("unexpected net_info response type: %q", resp.Type)
	}
	var info NetInfo
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		return nil, fmt.Errorf("decode net_info response: %w", err)
	}
	return &info, nil
}

// Mount mounts a filesystem inside the guest.
//
// Use this rather than Exec'ing a `mount` shell string. The agent performs it
// in the root mount namespace, which is rshared, so the mount propagates into
// the inner container that user code runs in — and into any container spawned
// after a respawn. A mount performed via Exec would land inside whichever
// namespace ran the command and would silently disappear when that namespace
// was replaced, leaving an empty directory where a volume used to be.
func (c *Client) Mount(ctx context.Context, source, target, fstype, data string, mkdir bool) error {
	req := &request{
		Type: reqMount,
		ID:   newID(),
		Mount: &mountPayload{
			Source: source,
			Target: target,
			FSType: fstype,
			Data:   data,
			MkDir:  mkdir,
		},
	}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return err
	}
	if resp.Type == respError {
		return fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respOK {
		return fmt.Errorf("unexpected mount response type: %q", resp.Type)
	}
	return nil
}

// Bounce restarts user code inside the guest without rebooting the VM.
//
// Processes, PTYs, IPC objects and inner mounts reset; every file persists (the
// rootfs is shared, not an overlay), as do routes, iptables rules and anything
// in the root namespace. In-flight exec sessions are lost; this connection and
// the host's port forwards survive, which is the point of bouncing rather than
// restarting the VM.
func (c *Client) Bounce(ctx context.Context) error {
	req := &request{Type: reqBounce, ID: newID()}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return err
	}
	if resp.Type == respError {
		return fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respOK {
		return fmt.Errorf("unexpected bounce response type: %q", resp.Type)
	}
	return nil
}

// ReadFile reads a file from inside the guest.
//
// The agent has had file handlers since the beginning; nothing on the host ever
// called them, so every read went through exec and a shell. This is the direct
// path: no shell quoting, no output parsing, and binary-safe.
func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error) {
	req := &request{Type: reqReadFile, ID: newID(), File: &filePayload{Path: path}}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return nil, err
	}
	if resp.Type == respError {
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	return resp.Data, nil
}

// WriteFile writes a file inside the guest, creating parent directories.
// mode 0 means 0644.
func (c *Client) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	req := &request{Type: reqWriteFile, ID: newID(),
		File: &filePayload{Path: path, Content: content, Mode: mode}}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return err
	}
	if resp.Type == respError {
		return fmt.Errorf("agent error: %s", resp.Error)
	}
	return nil
}

// ListDir lists a directory inside the guest.
func (c *Client) ListDir(ctx context.Context, path string) ([]DirEntry, error) {
	req := &request{Type: reqListDir, ID: newID(), File: &filePayload{Path: path}}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return nil, err
	}
	if resp.Type == respError {
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	var out []DirEntry
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return nil, fmt.Errorf("decode listing: %w", err)
		}
	}
	return out, nil
}

// DeleteFile removes a file or empty directory inside the guest.
func (c *Client) DeleteFile(ctx context.Context, path string) error {
	req := &request{Type: reqDeleteFile, ID: newID(), File: &filePayload{Path: path}}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return err
	}
	if resp.Type == respError {
		return fmt.Errorf("agent error: %s", resp.Error)
	}
	return nil
}

// ServiceAdd declares a service and starts supervising it.
func (c *Client) ServiceAdd(ctx context.Context, name, run, workdir, restart string, env map[string]string) error {
	return c.serviceCall(ctx, reqServiceAdd, &servicePayload{
		Name: name, Run: run, WorkDir: workdir, Restart: restart, Env: env,
	})
}

// ServiceRemove stops supervising a service and terminates it.
func (c *Client) ServiceRemove(ctx context.Context, name string) error {
	return c.serviceCall(ctx, reqServiceRm, &servicePayload{Name: name})
}

// ServiceRestart forces one service to restart now.
func (c *Client) ServiceRestart(ctx context.Context, name string) error {
	return c.serviceCall(ctx, reqServiceRst, &servicePayload{Name: name})
}

// ServiceReconcile replaces the declared set with the given services.
//
// Idempotent, which is what lets one call serve boot, resume, restore and
// bounce: already-running services are left alone, missing ones are started,
// and anything no longer declared is stopped.
func (c *Client) ServiceReconcile(ctx context.Context, svcs []ServiceState) error {
	payload := &servicePayload{Reconcile: true}
	for _, s := range svcs {
		payload.Services = append(payload.Services, servicePayload{
			Name: s.Name, Run: s.Run, Restart: s.Restart,
		})
	}
	return c.serviceCall(ctx, reqServiceAdd, payload)
}

// ServiceLogs returns the most recent output of one service. tail <= 0 means
// everything retained.
//
// The buffer belongs to the supervisor outside the container, so logs survive a
// restart of the service and a bounce — the output explaining why something
// died is still there afterwards.
func (c *Client) ServiceLogs(ctx context.Context, name string, tail int) ([]LogLine, error) {
	req := &request{Type: reqServiceLog, ID: newID(), Service: &servicePayload{Name: name, Tail: tail}}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return nil, err
	}
	if resp.Type == respError {
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	var out []LogLine
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return nil, fmt.Errorf("decode service logs: %w", err)
		}
	}
	return out, nil
}

// ServiceList reports every supervised service.
func (c *Client) ServiceList(ctx context.Context) ([]ServiceState, error) {
	req := &request{Type: reqServiceLs, ID: newID()}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return nil, err
	}
	if resp.Type == respError {
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	var out []ServiceState
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return nil, fmt.Errorf("decode service list: %w", err)
		}
	}
	return out, nil
}

func (c *Client) serviceCall(ctx context.Context, verb string, p *servicePayload) error {
	req := &request{Type: verb, ID: newID(), Service: p}
	var resp response
	if err := c.exchange(ctx, req, &resp); err != nil {
		return err
	}
	if resp.Type == respError {
		return fmt.Errorf("agent error: %s", resp.Error)
	}
	if resp.Type != respOK {
		return fmt.Errorf("unexpected %s response type: %q", verb, resp.Type)
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

// ExecStreamFrame is one chunk of a streaming exec: output as it is produced, or the final exit.
type ExecStreamFrame struct {
	// Kind is "stdout", "stderr", or "exit".
	Kind     string
	Data     []byte
	ExitCode int
}

// ExecStream runs a command on the guest and delivers output frames to onFrame AS THEY ARRIVE,
// returning the exit code once the command finishes.
//
// The difference from Exec is the whole point: Exec waits for the command to finish and hands back
// one buffer, so a caller cannot watch a build or notice a command that has stopped producing
// output. It also merges stdout and stderr, because the buffered guest verb does. The guest's
// exec_stream handler has kept them separate all along (agent/internal/handler/exec_stream.go
// writes RespStdout and RespStderr frames) — that split was being discarded at this boundary.
//
// Holds one connection open for the command's lifetime, like ExecInteractive. Cancelling ctx closes
// the connection, which is what stops the command: the guest's stream handler notices its writes
// failing. There is no signal verb in the agent protocol to do it more precisely.
func (c *Client) ExecStream(ctx context.Context, command, stdin string, onFrame func(ExecStreamFrame) error) (int, error) {
	conn, err := c.dialer.Dial(ctx)
	if err != nil {
		return 0, fmt.Errorf("dial agent: %w", err)
	}
	defer conn.Close()

	// Close the connection when the caller gives up, so a cancelled request does not leave the
	// guest running a command nobody is reading.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	req := &request{
		Type: reqExecStream,
		ID:   newID(),
		Exec: &execPayload{Command: command, Stdin: stdin},
	}
	if err := writeFrame(conn, req); err != nil {
		return 0, fmt.Errorf("send exec_stream: %w", err)
	}

	for {
		var resp response
		if err := readFrame(conn, &resp); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("read exec_stream frame: %w", err)
		}
		switch resp.Type {
		case respStdout, respStderr:
			if ferr := onFrame(ExecStreamFrame{Kind: resp.Type, Data: resp.Data}); ferr != nil {
				return 0, ferr
			}
		case respExit:
			if ferr := onFrame(ExecStreamFrame{Kind: respExit, ExitCode: resp.ExitCode}); ferr != nil {
				return 0, ferr
			}
			return resp.ExitCode, nil
		case respError:
			return 0, fmt.Errorf("agent error: %s", resp.Error)
		default:
			return 0, fmt.Errorf("unexpected exec_stream response type: %q", resp.Type)
		}
	}
}
