package agentclient

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFirecracker is a unix-socket server that mimics Firecracker's
// vsock-to-Unix-socket bridge: it accepts a "CONNECT <port>\n" line,
// optionally rejects it, and otherwise hands the byte stream to a
// caller-supplied agent handler.
type fakeFirecracker struct {
	t            *testing.T
	path         string
	ln           net.Listener
	acceptPort   uint32 // only accept CONNECTs to this port; 0 means accept any
	rejectAlways bool
	agent        func(net.Conn) // runs after a successful handshake

	wg sync.WaitGroup
}

func newFakeFirecracker(t *testing.T, agent func(net.Conn)) *fakeFirecracker {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vsock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeFirecracker{
		t:     t,
		path:  path,
		ln:    ln,
		agent: agent,
	}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() {
		ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeFirecracker) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func(c net.Conn) {
			defer f.wg.Done()
			f.handle(c)
		}(conn)
	}
}

func (f *fakeFirecracker) handle(conn net.Conn) {
	br := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	line = strings.TrimRight(line, "\r\n")

	if !strings.HasPrefix(line, "CONNECT ") {
		fmt.Fprintf(conn, "ERROR bad command\n")
		conn.Close()
		return
	}
	if f.rejectAlways {
		fmt.Fprintf(conn, "ERROR rejected\n")
		conn.Close()
		return
	}
	var port uint32
	if _, err := fmt.Sscanf(line, "CONNECT %d", &port); err != nil {
		fmt.Fprintf(conn, "ERROR malformed port\n")
		conn.Close()
		return
	}
	if f.acceptPort != 0 && port != f.acceptPort {
		fmt.Fprintf(conn, "ERROR wrong port\n")
		conn.Close()
		return
	}

	if _, err := fmt.Fprintf(conn, "OK 5\n"); err != nil {
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})

	// Anything still buffered after the OK line would represent
	// a real client-side bug — our handshake reader is unbuffered,
	// so verify the buffer is empty before handing the conn over.
	if br.Buffered() > 0 {
		f.t.Errorf("fake server: %d bytes buffered after handshake (client should be unbuffered)", br.Buffered())
	}

	if f.agent != nil {
		f.agent(conn)
	}
	conn.Close()
}

// echoOnceAgent reads one length-prefixed JSON request and writes back a
// response built by respFn.
func echoOnceAgent(t *testing.T, respFn func(req map[string]interface{}) interface{}) func(net.Conn) {
	return func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		var hdr [4]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			t.Errorf("agent: read length: %v", err)
			return
		}
		size := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, size)
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Errorf("agent: read body: %v", err)
			return
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("agent: parse req: %v", err)
			return
		}
		resp := respFn(req)
		out, err := json.Marshal(resp)
		if err != nil {
			t.Errorf("agent: marshal resp: %v", err)
			return
		}
		var rh [4]byte
		binary.BigEndian.PutUint32(rh[:], uint32(len(out)))
		if _, err := conn.Write(rh[:]); err != nil {
			return
		}
		conn.Write(out)
	}
}

func TestFirecrackerVsockDialer_Handshake(t *testing.T) {
	server := newFakeFirecracker(t, nil)
	server.acceptPort = 5123

	dialer := &FirecrackerVsockDialer{UDSPath: server.path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
}

func TestFirecrackerVsockDialer_Handshake_WrongPort(t *testing.T) {
	server := newFakeFirecracker(t, nil)
	server.acceptPort = 9999 // not 5123

	dialer := &FirecrackerVsockDialer{UDSPath: server.path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := dialer.Dial(ctx)
	if err == nil {
		t.Fatal("expected handshake rejection, got nil")
	}
	if !strings.Contains(err.Error(), "rejected CONNECT") {
		t.Fatalf("expected 'rejected CONNECT' in error, got: %v", err)
	}
}

func TestFirecrackerVsockDialer_Handshake_Rejected(t *testing.T) {
	server := newFakeFirecracker(t, nil)
	server.rejectAlways = true

	dialer := &FirecrackerVsockDialer{UDSPath: server.path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := dialer.Dial(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFirecrackerVsockDialer_Dial_NoSocket(t *testing.T) {
	dialer := &FirecrackerVsockDialer{UDSPath: "/nonexistent/socket.sock"}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := dialer.Dial(ctx)
	if err == nil {
		t.Fatal("expected dial error for missing socket")
	}
}

func TestClient_Ping(t *testing.T) {
	server := newFakeFirecracker(t, echoOnceAgent(t, func(req map[string]interface{}) interface{} {
		if got := req["type"]; got != "ping" {
			t.Errorf("expected type=ping, got %v", got)
		}
		return map[string]interface{}{
			"type": "ok",
			"id":   req["id"],
		}
	}))

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestClient_Ping_AgentError(t *testing.T) {
	server := newFakeFirecracker(t, echoOnceAgent(t, func(req map[string]interface{}) interface{} {
		return map[string]interface{}{
			"type":  "error",
			"id":    req["id"],
			"error": "agent unhappy",
		}
	}))

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.Ping(ctx)
	if err == nil {
		t.Fatal("expected error from ping")
	}
	if !strings.Contains(err.Error(), "agent unhappy") {
		t.Fatalf("expected agent error in message, got: %v", err)
	}
}

func TestClient_Exec_Success(t *testing.T) {
	server := newFakeFirecracker(t, echoOnceAgent(t, func(req map[string]interface{}) interface{} {
		if got := req["type"]; got != "exec" {
			t.Errorf("expected type=exec, got %v", got)
		}
		exec, ok := req["exec"].(map[string]interface{})
		if !ok {
			t.Errorf("expected exec payload, got %T", req["exec"])
		}
		if got := exec["command"]; got != "echo hi" {
			t.Errorf("expected command='echo hi', got %v", got)
		}
		// Match the agent's wire format: Data is base64 in JSON.
		return map[string]interface{}{
			"type":      "exit",
			"id":        req["id"],
			"data":      []byte("hi\n"),
			"exit_code": 0,
		}
	}))

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := client.Exec(ctx, "echo hi", "")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Output != "hi\n" {
		t.Errorf("output = %q, want %q", res.Output, "hi\n")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

func TestClient_Exec_NonZeroExit(t *testing.T) {
	server := newFakeFirecracker(t, echoOnceAgent(t, func(req map[string]interface{}) interface{} {
		return map[string]interface{}{
			"type":      "exit",
			"id":        req["id"],
			"data":      []byte("oops"),
			"exit_code": 42,
		}
	}))

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := client.Exec(ctx, "false", "")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", res.ExitCode)
	}
	if res.Output != "oops" {
		t.Errorf("output = %q, want %q", res.Output, "oops")
	}
}

func TestClient_Exec_AgentError(t *testing.T) {
	server := newFakeFirecracker(t, echoOnceAgent(t, func(req map[string]interface{}) interface{} {
		return map[string]interface{}{
			"type":  "error",
			"id":    req["id"],
			"error": "no such workdir",
		}
	}))

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.Exec(ctx, "ls /nope", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no such workdir") {
		t.Fatalf("expected 'no such workdir' in error, got: %v", err)
	}
}

func TestClient_Exec_DialFailure(t *testing.T) {
	client := New(&FirecrackerVsockDialer{UDSPath: "/nonexistent/socket.sock"})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := client.Exec(ctx, "true", "")
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(err.Error(), "dial agent") {
		t.Fatalf("expected 'dial agent' in error, got: %v", err)
	}
}

func TestClient_Poweroff_Success(t *testing.T) {
	server := newFakeFirecracker(t, echoOnceAgent(t, func(req map[string]interface{}) interface{} {
		if got := req["type"]; got != "poweroff" {
			t.Errorf("expected type=poweroff, got %v", got)
		}
		return map[string]interface{}{
			"type": "ok",
			"id":   req["id"],
		}
	}))

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Poweroff(ctx); err != nil {
		t.Fatalf("poweroff: %v", err)
	}
}

func TestClient_Poweroff_ConnectionTeardown(t *testing.T) {
	// Server that closes the connection immediately after reading the
	// request — simulating the guest shutting down mid-response.
	server := newFakeFirecracker(t, func(conn net.Conn) {
		var hdr [4]byte
		io.ReadFull(conn, hdr[:])
		size := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, size)
		io.ReadFull(conn, body)
		// Close without sending a response — the guest powered off.
		conn.Close()
	})

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Should succeed: connection teardown is treated as successful poweroff.
	if err := client.Poweroff(ctx); err != nil {
		t.Fatalf("poweroff should succeed on connection teardown, got: %v", err)
	}
}

func TestClient_Poweroff_Timeout(t *testing.T) {
	// Server that hangs forever after reading the request — simulating
	// a guest that doesn't respond to poweroff.
	server := newFakeFirecracker(t, func(conn net.Conn) {
		var hdr [4]byte
		io.ReadFull(conn, hdr[:])
		size := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, size)
		io.ReadFull(conn, body)
		// Hang — don't respond, don't close.
		buf := make([]byte, 1)
		conn.Read(buf)
	})

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Should fail: a timeout is NOT treated as successful poweroff.
	err := client.Poweroff(ctx)
	if err == nil {
		t.Fatal("poweroff should fail on timeout, got nil")
	}
}

func TestClient_Exec_ContextCancel(t *testing.T) {
	// Server that hangs forever after handshake — ctx cancel must unblock the call.
	server := newFakeFirecracker(t, func(conn net.Conn) {
		// Read header + body, then sit on the conn doing nothing.
		var hdr [4]byte
		io.ReadFull(conn, hdr[:])
		size := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, size)
		io.ReadFull(conn, body)
		// Sit until conn is closed.
		buf := make([]byte, 1)
		conn.Read(buf)
	})

	client := New(&FirecrackerVsockDialer{UDSPath: server.path})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.Exec(ctx, "sleep 10", "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected fast cancel, took %v", elapsed)
	}
	// The error is some net.Error timeout wrapped in a read-response frame.
	var ne net.Error
	if !errors.As(err, &ne) {
		t.Logf("note: error was not a net.Error (got %T: %v) — that's fine as long as it surfaced", err, err)
	}
}
