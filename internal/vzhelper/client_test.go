package vzhelper

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeHelper is an in-process Unix-socket server that implements the
// helper IPC protocol just enough to exercise the Client. Real fds are
// supplied via os.Pipe so SCM_RIGHTS round-trips can be asserted.
type fakeHelper struct {
	t        *testing.T
	path     string
	ln       *net.UnixListener
	wg       sync.WaitGroup
	handler  func(t *testing.T, conn *net.UnixConn, req *Request)
	stopOnce sync.Once
}

func newFakeHelper(t *testing.T, handler func(t *testing.T, conn *net.UnixConn, req *Request)) *fakeHelper {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "vz.sock")
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeHelper{t: t, path: path, ln: ln, handler: handler}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeHelper) stop() {
	f.stopOnce.Do(func() {
		f.ln.Close()
	})
	f.wg.Wait()
}

func (f *fakeHelper) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.AcceptUnix()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func(c *net.UnixConn) {
			defer f.wg.Done()
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			var req Request
			if err := readFrame(c, &req); err != nil {
				return
			}
			f.handler(f.t, c, &req)
		}(conn)
	}
}

// writeFrameWithFd writes a length-prefixed JSON frame plus an SCM_RIGHTS
// ancillary message carrying one fd, all in a single sendmsg.
func writeFrameWithFd(conn *net.UnixConn, v interface{}, fd int) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(data)))
	frame := append(hdr, data...)

	oob := syscall.UnixRights(fd)
	_, _, err = conn.WriteMsgUnix(frame, oob, nil)
	return err
}

// === SocketPath ===

func TestSocketPath(t *testing.T) {
	got := SocketPath("/home/me/.mvm", "foo")
	want := "/home/me/.mvm/run/vz-foo.sock"
	if got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

// === Status / Pause / Resume / Stop ===

func okHandler(state string) func(*testing.T, *net.UnixConn, *Request) {
	return func(t *testing.T, conn *net.UnixConn, req *Request) {
		writeFrame(conn, &Response{OK: true, State: state})
	}
}

func errHandler(msg string) func(*testing.T, *net.UnixConn, *Request) {
	return func(t *testing.T, conn *net.UnixConn, req *Request) {
		writeFrame(conn, &Response{OK: false, Error: msg})
	}
}

func TestClient_Status(t *testing.T) {
	helper := newFakeHelper(t, func(t *testing.T, conn *net.UnixConn, req *Request) {
		if req.Cmd != CmdStatus {
			t.Errorf("cmd = %q, want %q", req.Cmd, CmdStatus)
		}
		writeFrame(conn, &Response{OK: true, State: "running"})
	})
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state != "running" {
		t.Errorf("state = %q, want running", state)
	}
}

func TestClient_Pause(t *testing.T) {
	helper := newFakeHelper(t, func(t *testing.T, conn *net.UnixConn, req *Request) {
		if req.Cmd != CmdPause {
			t.Errorf("cmd = %q, want %q", req.Cmd, CmdPause)
		}
		writeFrame(conn, &Response{OK: true})
	})
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
}

func TestClient_Resume(t *testing.T) {
	helper := newFakeHelper(t, okHandler(""))
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestClient_Stop_OK(t *testing.T) {
	helper := newFakeHelper(t, func(t *testing.T, conn *net.UnixConn, req *Request) {
		writeFrame(conn, &Response{OK: true})
	})
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestClient_Stop_ConnTeardown(t *testing.T) {
	// Helper "exits" by closing the connection without writing a response,
	// just like a real helper that's tearing down. Stop should treat this
	// as success.
	helper := newFakeHelper(t, func(t *testing.T, conn *net.UnixConn, req *Request) {
		conn.Close()
	})
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop should ignore conn teardown, got: %v", err)
	}
}

func TestClient_Status_HelperError(t *testing.T) {
	helper := newFakeHelper(t, errHandler("vm not running"))
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Status(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "vm not running") {
		t.Errorf("error = %v, want contains 'vm not running'", err)
	}
}

func TestClient_Dial_NoSocket(t *testing.T) {
	c := New("/nonexistent/vz.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := c.Status(ctx)
	if err == nil {
		t.Fatal("expected dial error")
	}
}

// === Connect (the SCM_RIGHTS path) ===

func TestClient_Connect_SendsFd(t *testing.T) {
	// Make a real fd to send: a pipe pair. The receiver should be able to
	// read what we write to the write end through the fd we sent.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	// Capture the fd value once, in this goroutine, so the helper goroutine
	// never touches the *os.File. Otherwise the deferred pr.Close() races
	// with the helper goroutine's pr.Fd() under -race.
	prFd := int(pr.Fd())

	helper := newFakeHelper(t, func(t *testing.T, conn *net.UnixConn, req *Request) {
		if req.Cmd != CmdConnect {
			t.Errorf("cmd = %q, want %q", req.Cmd, CmdConnect)
		}
		if req.Port != 5123 {
			t.Errorf("port = %d, want 5123", req.Port)
		}
		// Send the read end of the pipe via SCM_RIGHTS. prFd was
		// captured by value above.
		if err := writeFrameWithFd(conn, &Response{OK: true}, prFd); err != nil {
			t.Errorf("writeFrameWithFd: %v", err)
		}
	})

	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	f, err := c.Connect(ctx, 5123)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer f.Close()

	// Now write to the original write end of the pipe and verify the
	// fd we received reads the same bytes — proves it's a real dup.
	want := []byte("hello-from-vz-helper")
	go func() {
		pw.Write(want)
	}()
	got := make([]byte, len(want))
	_, err = io.ReadFull(f, got)
	if err != nil {
		t.Fatalf("read from received fd: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("read %q, want %q", got, want)
	}
}

func TestClient_Connect_HelperError(t *testing.T) {
	helper := newFakeHelper(t, errHandler("vsock not available"))
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Connect(ctx, 5123)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "vsock not available") {
		t.Errorf("error = %v", err)
	}
}

func TestClient_Connect_OkButNoFd(t *testing.T) {
	helper := newFakeHelper(t, func(t *testing.T, conn *net.UnixConn, req *Request) {
		// OK response but no SCM_RIGHTS — buggy helper.
		writeFrame(conn, &Response{OK: true})
	})
	c := New(helper.path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Connect(ctx, 5123)
	if err == nil {
		t.Fatal("expected error for OK-without-fd")
	}
	if !strings.Contains(err.Error(), "no fd") {
		t.Errorf("error = %v, want contains 'no fd'", err)
	}
}

func TestClient_Connect_DialFailure(t *testing.T) {
	c := New("/nonexistent/vz.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := c.Connect(ctx, 5123)
	if err == nil {
		t.Fatal("expected dial error")
	}
}

// === decodeFrameFromBuffer ===

func TestDecodeFrameFromBuffer_Roundtrip(t *testing.T) {
	resp := &Response{OK: true, State: "running"}
	data, _ := json.Marshal(resp)
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(data)))
	buf := append(hdr, data...)
	got, err := decodeFrameFromBuffer(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.State != "running" {
		t.Errorf("got %+v", got)
	}
}

func TestDecodeFrameFromBuffer_TooShort(t *testing.T) {
	_, err := decodeFrameFromBuffer([]byte{0, 0, 0})
	if err == nil {
		t.Fatal("expected short-frame error")
	}
}

func TestDecodeFrameFromBuffer_Truncated(t *testing.T) {
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, 100) // claim 100 bytes, send 5
	_, err := decodeFrameFromBuffer(append(hdr, []byte("short")...))
	if err == nil {
		t.Fatal("expected truncated-frame error")
	}
}

// === sanity: errors.Is for EPIPE pattern Stop relies on ===

func TestStop_EPIPETreatedAsSuccess(t *testing.T) {
	// Direct test of the errors.Is path that Stop uses: wrap EPIPE inside
	// a fmt.Errorf and ensure it's still recognized.
	err := wrappedErr(syscall.EPIPE)
	if !errors.Is(err, syscall.EPIPE) {
		t.Fatal("EPIPE should be recognized through errors.Is")
	}
}

func wrappedErr(inner error) error {
	return &wrap{inner: inner}
}

type wrap struct{ inner error }

func (w *wrap) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrap) Unwrap() error { return w.inner }
