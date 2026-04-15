package agentclient

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/vzhelper"
)

// fakeVZHelper is a minimal mvm-vz helper IPC server for end-to-end
// dialer tests. It accepts one Connect frame from a client, allocates
// a socketpair, sends one end back via SCM_RIGHTS (looking like the
// vsock fd Apple-VF would hand us), and runs agentFn against the
// other end so the test can speak agent protocol back to the dialer.
type fakeVZHelper struct {
	t       *testing.T
	path    string
	ln      *net.UnixListener
	wg      sync.WaitGroup
	agentFn func(net.Conn)
}

func newFakeVZHelper(t *testing.T, agentFn func(net.Conn)) *fakeVZHelper {
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
	f := &fakeVZHelper{t: t, path: path, ln: ln, agentFn: agentFn}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() {
		ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeVZHelper) serve() {
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

			// Read one length-prefixed JSON request.
			var hdr [4]byte
			if _, err := io.ReadFull(c, hdr[:]); err != nil {
				return
			}
			size := binary.BigEndian.Uint32(hdr[:])
			body := make([]byte, size)
			if _, err := io.ReadFull(c, body); err != nil {
				return
			}
			var req vzhelper.Request
			if err := json.Unmarshal(body, &req); err != nil {
				return
			}
			if req.Cmd != vzhelper.CmdConnect {
				return
			}

			// Make a real socketpair. fds[0] becomes the "vsock fd" the
			// helper hands to the client. fds[1] stays here as the
			// "agent" end that this test drives.
			fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				f.t.Errorf("socketpair: %v", err)
				return
			}

			// Wrap fds[1] as net.Conn for the agent side. net.FileConn
			// dups the fd internally, so we can close our copy of the file.
			agentFile := os.NewFile(uintptr(fds[1]), "vz-test-agent-side")
			agentConn, err := net.FileConn(agentFile)
			agentFile.Close()
			if err != nil {
				f.t.Errorf("FileConn: %v", err)
				syscall.Close(fds[0])
				return
			}

			// Send fds[0] to the client via SCM_RIGHTS.
			respJSON, _ := json.Marshal(&vzhelper.Response{OK: true})
			respHdr := make([]byte, 4)
			binary.BigEndian.PutUint32(respHdr, uint32(len(respJSON)))
			frame := append(respHdr, respJSON...)
			oob := syscall.UnixRights(fds[0])
			if _, _, err := c.WriteMsgUnix(frame, oob, nil); err != nil {
				f.t.Errorf("WriteMsgUnix: %v", err)
				syscall.Close(fds[0])
				agentConn.Close()
				return
			}
			// Close our reference; the client now owns the dup.
			syscall.Close(fds[0])

			// Drive the agent side until agentFn returns.
			f.agentFn(agentConn)
			agentConn.Close()
		}(conn)
	}
}

// === Tests ===

func TestVZSocketDialer_String(t *testing.T) {
	d := &VZSocketDialer{SocketPath: "/tmp/foo.sock"}
	got := d.String()
	if got != "vz-helper(/tmp/foo.sock, port=5123)" {
		t.Errorf("String() = %q", got)
	}

	d2 := &VZSocketDialer{SocketPath: "/tmp/foo.sock", GuestPort: 9999}
	got = d2.String()
	if got != "vz-helper(/tmp/foo.sock, port=9999)" {
		t.Errorf("String() = %q", got)
	}
}

func TestVZSocketDialer_Dial_NoSocket(t *testing.T) {
	d := &VZSocketDialer{SocketPath: "/nonexistent/vz.sock"}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := d.Dial(ctx)
	if err == nil {
		t.Fatal("expected dial error")
	}
}

// TestVZSocketDialer_FullChain_Ping is the end-to-end happy path:
//
//   - Client.Ping
//     → VZSocketDialer.Dial
//     → vzhelper.Client.Connect (writes CmdConnect frame)
//     → fakeVZHelper accepts, makes a socketpair, sends one end via SCM_RIGHTS
//     → vzhelper.Client extracts the fd, returns *os.File
//     → VZSocketDialer wraps it as net.Conn
//     → Client writes a Ping frame over it
//     → fakeVZHelper's agentFn reads Ping, writes ok back
//     → Client.Ping returns success
//
// This proves the SCM_RIGHTS path produces a real bidirectional fd that
// the agent protocol can flow over without anyone in the middle knowing
// or caring that it started life as a vsock connection in another process.
func TestVZSocketDialer_FullChain_Ping(t *testing.T) {
	helper := newFakeVZHelper(t, func(agent net.Conn) {
		// Read one length-prefixed JSON frame from the "guest agent" side.
		var hdr [4]byte
		if _, err := io.ReadFull(agent, hdr[:]); err != nil {
			t.Errorf("agent read header: %v", err)
			return
		}
		size := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, size)
		if _, err := io.ReadFull(agent, body); err != nil {
			t.Errorf("agent read body: %v", err)
			return
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("agent parse req: %v", err)
			return
		}
		if req["type"] != "ping" {
			t.Errorf("agent req type = %v, want ping", req["type"])
			return
		}

		// Reply with an ok response in agent protocol format.
		resp := map[string]interface{}{
			"type": "ok",
			"id":   req["id"],
		}
		respBody, _ := json.Marshal(resp)
		respHdr := make([]byte, 4)
		binary.BigEndian.PutUint32(respHdr, uint32(len(respBody)))
		agent.Write(respHdr)
		agent.Write(respBody)
	})

	dialer := &VZSocketDialer{SocketPath: helper.path}
	client := New(dialer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestVZSocketDialer_FullChain_Exec exercises the Exec round-trip
// over the same SCM_RIGHTS-piped fd path.
func TestVZSocketDialer_FullChain_Exec(t *testing.T) {
	helper := newFakeVZHelper(t, func(agent net.Conn) {
		var hdr [4]byte
		if _, err := io.ReadFull(agent, hdr[:]); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, size)
		if _, err := io.ReadFull(agent, body); err != nil {
			return
		}
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		exec, _ := req["exec"].(map[string]interface{})
		if exec["command"] != "echo hi" {
			t.Errorf("command = %v", exec["command"])
		}

		resp := map[string]interface{}{
			"type":      "exit",
			"id":        req["id"],
			"data":      []byte("hi\n"),
			"exit_code": 0,
		}
		respBody, _ := json.Marshal(resp)
		respHdr := make([]byte, 4)
		binary.BigEndian.PutUint32(respHdr, uint32(len(respBody)))
		agent.Write(respHdr)
		agent.Write(respBody)
	})

	dialer := &VZSocketDialer{SocketPath: helper.path}
	client := New(dialer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := client.Exec(ctx, "echo hi", "")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Output != "hi\n" {
		t.Errorf("output = %q, want 'hi\\n'", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d", res.ExitCode)
	}
}
