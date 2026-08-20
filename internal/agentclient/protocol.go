// Package agentclient is a host-side client for the in-guest mvm-agent.
//
// It speaks the same length-prefixed JSON framing as the agent
// (see agent/internal/protocol). Each method opens a fresh connection
// via the Dialer, exchanges one request/response, and closes — keeping
// the client stateless and free of connection-management bugs.
//
// Today the only Dialer is FirecrackerVsockDialer, which dials Firecracker's
// vsock-over-Unix-socket bridge. Future Dialers (VZ helper SCM_RIGHTS,
// TCP debug fallback) plug in without changing call sites.
package agentclient

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Wire-format request types — must match agent/internal/protocol.
const (
	reqPing       = "ping"
	reqExec       = "exec"
	reqExecPty    = "exec_pty"
	reqPoweroff   = "poweroff"
	reqTCPForward = "tcp_forward"
	reqNetInfo    = "net_info"
	reqMount      = "mount"
	reqBounce     = "bounce"
	reqServiceAdd = "service_add"
	reqServiceRm  = "service_rm"
	reqServiceLs  = "service_ls"
	reqServiceRst = "service_restart"
)

// Wire-format response types — must match agent/internal/protocol.
const (
	respOK     = "ok"
	respError  = "error"
	respExit   = "exit"
	respStdout = "stdout"
	respStdin  = "stdin"  // host→agent: terminal input
	respResize = "resize" // host→agent: window resize (exit_code = rows<<16|cols)
)

const maxFrameSize = 10 * 1024 * 1024 // 10 MiB, matches agent

// request is the wire-format request envelope.
type request struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Exec    *execPayload    `json:"exec,omitempty"`
	Pty     *ptyPayload     `json:"pty,omitempty"`
	Forward *forwardPayload `json:"forward,omitempty"`
	Mount   *mountPayload   `json:"mount,omitempty"`
	Service *servicePayload `json:"service,omitempty"`
}

// servicePayload matches agent/internal/protocol.ServiceRequest.
type servicePayload struct {
	Name      string            `json:"name"`
	Run       string            `json:"run,omitempty"`
	WorkDir   string            `json:"workdir,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Restart   string            `json:"restart,omitempty"`
	Reconcile bool              `json:"reconcile,omitempty"`
	Services  []servicePayload  `json:"services,omitempty"`
}

// ServiceState is one supervised service as reported by ServiceList.
type ServiceState struct {
	Name     string `json:"name"`
	Run      string `json:"run"`
	Restart  string `json:"restart,omitempty"`
	Running  bool   `json:"running"`
	Restarts int    `json:"restarts"`
	LastExit int    `json:"last_exit"`
}

// mountPayload matches agent/internal/protocol.MountRequest.
type mountPayload struct {
	Source string `json:"source"`
	Target string `json:"target"`
	FSType string `json:"fstype"`
	Data   string `json:"data,omitempty"`
	MkDir  bool   `json:"mkdir,omitempty"`
}

// forwardPayload matches agent/internal/protocol.ForwardRequest.
type forwardPayload struct {
	Port int `json:"port"`
}

// NetInfo is the guest's self-reported network configuration — the JSON
// payload carried in a net_info response's Data field. Matches
// agent/internal/protocol.NetInfo.
type NetInfo struct {
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
}

type execPayload struct {
	Command string            `json:"command"`
	Stdin   string            `json:"stdin,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
}

// ptyPayload matches agent/internal/protocol.ExecPtyRequest.
type ptyPayload struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Rows    uint16            `json:"rows"`
	Cols    uint16            `json:"cols"`
	Term    string            `json:"term,omitempty"`
}

// response is the wire-format response envelope.
type response struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// writeFrame writes a length-prefixed JSON frame in a single Write call.
// See internal/vzhelper/protocol.go for why single-Write matters for the
// SCM_RIGHTS / ReadMsgUnix path. Note: single-Write atomicity on
// SOCK_STREAM is not a POSIX guarantee — it works because our frames
// are well under the kernel's socket send buffer size.
func writeFrame(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if len(data) > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes", len(data))
	}
	buf := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
	copy(buf[4:], data)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// readFrame reads a length-prefixed JSON frame.
func readFrame(r io.Reader, v interface{}) error {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return fmt.Errorf("read length: %w", err)
	}
	size := binary.BigEndian.Uint32(hdr)
	if size > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("read data: %w", err)
	}
	return json.Unmarshal(data, v)
}

// --- Exported frame helpers for interactive exec (HTTP hijack relay) ---

// WriteFrame writes a length-prefixed JSON frame. Exported for use by the
// daemon's interactive exec relay and the CLI client.
func WriteFrame(w io.Writer, v interface{}) error { return writeFrame(w, v) }

// ReadFrame reads a length-prefixed JSON frame. Exported for use by the
// daemon's interactive exec relay and the CLI client.
func ReadFrame(r io.Reader, v interface{}) error { return readFrame(r, v) }

// NewID returns a short hex request ID. Exported for the daemon relay.
func NewID() string { return newID() }

// --- exec_pty wire types ---

// ExecPtyRequest is the wire-format request for interactive PTY exec.
type ExecPtyRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Command string `json:"command"`
}

// ExecPtyResponse is the initial response from the agent for exec_pty.
type ExecPtyResponse struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
}

// PtyFrame is a length-prefixed JSON frame exchanged during an interactive
// PTY session. Direction depends on Type:
//
//	Agent → Host: "stdout" (Data = PTY output), "exit" (ExitCode set)
//	Host → Agent: "stdin"  (Data = user input),  "resize" (Rows/Cols set)
type PtyFrame struct {
	Type     string `json:"type"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Cols     int    `json:"cols,omitempty"`
}
