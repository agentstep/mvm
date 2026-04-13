package handler

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandleExecStream runs a command and streams stdout/stderr frames to the writer.
// Sends multiple Response frames: RespStdout/RespStderr during execution,
// then a final RespExit frame with the exit code.
func HandleExecStream(w io.Writer, req *protocol.ExecRequest, id string) {
	cmd := exec.Command("sh", "-c", req.Command)

	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		writeError(w, id, err)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		writeError(w, id, err)
		return
	}

	if err := cmd.Start(); err != nil {
		writeError(w, id, err)
		return
	}

	// Serialize frame writes — both goroutines write to the same connection
	var mu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		streamPipe(&mu, w, stdoutPipe, protocol.RespStdout, id)
	}()
	go func() {
		defer wg.Done()
		streamPipe(&mu, w, stderrPipe, protocol.RespStderr, id)
	}()

	wg.Wait()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	mu.Lock()
	protocol.WriteFrame(w, &protocol.Response{
		Type:     protocol.RespExit,
		ID:       id,
		ExitCode: exitCode,
	})
	mu.Unlock()
}

func streamPipe(mu *sync.Mutex, w io.Writer, r io.Reader, respType string, id string) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			mu.Lock()
			protocol.WriteFrame(w, &protocol.Response{
				Type: respType,
				ID:   id,
				Data: data,
			})
			mu.Unlock()
		}
		if err != nil {
			break
		}
	}
}

func writeError(w io.Writer, id string, err error) {
	protocol.WriteFrame(w, &protocol.Response{
		Type:  protocol.RespError,
		ID:    id,
		Error: err.Error(),
	})
}
