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
	length := make([]byte, 4)
	if _, err := io.ReadFull(r, length); err != nil {
		return fmt.Errorf("read length: %w", err)
	}
	size := binary.BigEndian.Uint32(length)
	if size > 10*1024*1024 { // 10MB max
		return fmt.Errorf("frame too large: %d bytes", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("read data: %w", err)
	}
	return json.Unmarshal(data, v)
}
