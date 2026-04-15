// Package vzhelper is a Go-side client for the per-VM mvm-vz Swift helper.
//
// The Swift helper is the long-lived process spawned by `mvm-vz create
// --foreground` that holds a single VZVirtualMachine and listens on a
// per-VM Unix socket (~/.mvm/run/vz-<name>.sock). This package speaks
// that protocol from Go.
//
// Wire format
//
// All messages are length-prefixed JSON: a 4-byte big-endian length
// followed by the JSON payload. This mirrors the agent protocol and
// internal/agentclient on purpose — no second framing convention to
// remember.
//
// One method is special: Connect dials the helper, asks it to open a
// vsock connection to the in-guest agent on a given port, and reads
// back a real file descriptor via SCM_RIGHTS ancillary data. The
// returned fd is a duplicate (the kernel ref-counts the underlying
// file), so the caller owns it and must Close it.
//
// Concurrency
//
// A Client is safe for concurrent use — calls are serialized by an
// internal mutex. Concurrent callers block each other since each call
// holds a connection for one request/response exchange. For higher
// parallelism, construct one Client per goroutine; SocketPath is cheap
// and the dial is sub-millisecond.
package vzhelper

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Wire-format command names.
const (
	CmdConnect = "connect"
	CmdPause   = "pause"
	CmdResume  = "resume"
	CmdStop    = "stop"
	CmdStatus  = "status"
)

const maxFrameSize = 1 * 1024 * 1024 // 1 MiB — control plane messages are tiny

// Request is the wire-format request envelope sent to the helper.
type Request struct {
	Cmd  string `json:"cmd"`
	Port uint32 `json:"port,omitempty"` // for CmdConnect only
}

// Response is the wire-format response envelope returned by the helper.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	State string `json:"state,omitempty"` // for CmdStatus
}

// SocketPath returns the per-VM helper IPC socket path for a given VM name
// inside the given mvm data dir (typically ~/.mvm).
//
// The helper creates the parent directory itself; callers should not assume
// it exists before the helper has started.
func SocketPath(mvmDir, vmName string) string {
	return filepath.Join(mvmDir, "run", "vz-"+vmName+".sock")
}

// HelperBinary locates the mvm-vz helper binary by name. It looks first
// next to the running mvm executable, then on PATH. Returns "mvm-vz" if
// no specific path is found — exec.LookPath will resolve from PATH at
// invocation time.
func HelperBinary() string {
	if execPath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(execPath), "mvm-vz")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "mvm-vz"
}

// writeFrame writes a length-prefixed JSON frame in a single Write call.
//
// The single-Write contract matters for readers that use ReadMsgUnix on
// a SOCK_STREAM Unix socket: ReadMsgUnix returns whatever is currently
// in the kernel buffer without waiting for a full frame, so a header-
// then-body sender can hand the reader half a frame and wedge it. With
// one Write the kernel queues the whole frame atomically.
//
// Note: POSIX does not guarantee atomicity for SOCK_STREAM writes of
// any size (only SOCK_SEQPACKET preserves message boundaries). This
// works because control-plane messages are well under the kernel's
// socket send buffer (typically 128KB+). Do not rely on this for large
// frames — the agentclient protocol allows up to 10 MiB, which could
// be split across multiple kernel buffers on a stream socket.
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
