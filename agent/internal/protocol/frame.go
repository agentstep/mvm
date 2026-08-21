package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Request types
const (
	ReqPing       = "ping"
	ReqExec       = "exec"
	ReqExecStream = "exec_stream"
	ReqExecPty    = "exec_pty"
	ReqWriteFile  = "write_file"
	ReqReadFile   = "read_file"
	ReqPoweroff   = "poweroff"
	ReqSetupNet   = "setup_network"
	ReqTCPForward = "tcp_forward"
	ReqNetInfo    = "net_info"
	ReqMount      = "mount"
	ReqBounce     = "bounce"
	ReqServiceAdd = "service_add"
	ReqServiceRm  = "service_rm"
	ReqServiceLs  = "service_ls"
	ReqServiceRst = "service_restart"
	ReqServiceLog = "service_logs"
	ReqListDir    = "list_dir"
	ReqDeleteFile = "delete_file"
)

// Response types
const (
	RespOK     = "ok"
	RespError  = "error"
	RespStdout = "stdout"
	RespStderr = "stderr"
	RespExit   = "exit"
	RespStdin  = "stdin"  // client→agent: stdin data
	RespResize = "resize" // client→agent: terminal resize
)

type Request struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Exec    *ExecRequest    `json:"exec,omitempty"`
	Pty     *ExecPtyRequest `json:"pty,omitempty"`
	File    *FileRequest    `json:"file,omitempty"`
	Network *NetworkRequest `json:"network,omitempty"`
	Forward *ForwardRequest `json:"forward,omitempty"`
	Mount   *MountRequest   `json:"mount,omitempty"`
	Service *ServiceRequest `json:"service,omitempty"`
}

// MountRequest asks the agent to mount a filesystem in the guest.
//
// This replaces mounting via `exec "mkdir -p X && mount -t virtiofs tagN X"`.
// That worked, but the mount was opaque shell text the agent had no record of,
// so nothing could re-establish it — and once exec runs inside the inner
// container, such a mount would land only in that namespace and silently
// disappear when the container respawned, leaving an empty directory where a
// volume used to be.
//
// Handled in the ROOT namespace, which (being rshared) propagates the mount
// into the current inner container and any future one.
type MountRequest struct {
	Source string `json:"source"`          // e.g. a virtiofs tag
	Target string `json:"target"`          // absolute path in the guest
	FSType string `json:"fstype"`          // e.g. "virtiofs"
	Data   string `json:"data,omitempty"`  // fs-specific options
	MkDir  bool   `json:"mkdir,omitempty"` // create Target first
}

// ServiceRequest carries a service declaration or a reference to one.
//
// Services are supervised from the ROOT namespace and run inside the container,
// which is what lets them survive a bounce: a supervisor inside the namespace
// it supervises dies with it.
type ServiceRequest struct {
	Name    string            `json:"name"`
	Run     string            `json:"run,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Restart string            `json:"restart,omitempty"`
	Tail    int               `json:"tail,omitempty"` // logs: line count, 0 = all
	// Reconcile, when set with Services, replaces the whole declared set.
	Reconcile bool             `json:"reconcile,omitempty"`
	Services  []ServiceRequest `json:"services,omitempty"`
}

// ForwardRequest asks the agent to connect to a TCP port on the guest's own
// loopback and then relay raw bytes over this connection. The target is always
// 127.0.0.1 inside the guest — the caller never supplies a host/IP, so the
// agent can't be used to reach anything but the guest's own services.
type ForwardRequest struct {
	Port int `json:"port"`
}

type ExecRequest struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Stdin   string            `json:"stdin,omitempty"`
}

type ExecPtyRequest struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Rows    uint16            `json:"rows"`
	Cols    uint16            `json:"cols"`
	Term    string            `json:"term,omitempty"`
}

type FileRequest struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
}

type NetworkRequest struct {
	DefaultGateway string `json:"default_gateway"`
	DNS            string `json:"dns"`
}

// NetInfo is the guest's self-reported network configuration, returned as
// JSON in a Response's Data field for a net_info request. It's discovered
// entirely via the Go net package and /proc/net/route — no ip/ifconfig
// binary is required in the guest image (the applevz base image ships
// neither; see internal/cli/start.go for why this matters: the guest gets
// its address via kernel-level DHCP against Apple's VZNAT device, and the
// host has no other way to learn what address was actually assigned).
type NetInfo struct {
	IP      string `json:"ip"`      // eth0's IPv4 address, "" if unconfigured
	Gateway string `json:"gateway"` // default route gateway, "" if none
}

type Response struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// WriteFrame writes a length-prefixed JSON frame.
func WriteFrame(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	// 4-byte big-endian length prefix
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	if _, err := w.Write(length); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

// ReadFrame reads a length-prefixed JSON frame.
func ReadFrame(r io.Reader, v interface{}) error {
	data, err := ReadRawFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ReadRawFrame reads a length-prefixed frame and returns its undecoded payload.
//
// The agent needs the raw bytes as well as the decoded request: it decodes to
// decide whether a request belongs in the inner container, and if it does, the
// original frame is forwarded verbatim alongside the connection's file
// descriptor. Re-encoding the decoded struct instead would silently drop any
// field this build does not know about, which is exactly the wrong behaviour
// across a version skew.
func ReadRawFrame(r io.Reader) ([]byte, error) {
	length := make([]byte, 4)
	if _, err := io.ReadFull(r, length); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}
	size := binary.BigEndian.Uint32(length)
	if size > 10*1024*1024 { // 10MB max
		return nil, fmt.Errorf("frame too large: %d bytes", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}
	return data, nil
}
