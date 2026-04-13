package handler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandleExec runs a command and returns the result.
func HandleExec(req *protocol.ExecRequest) *protocol.Response {
	cmd := exec.Command("sh", "-c", req.Command)

	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}

	// Pipe stdin if provided
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &protocol.Response{
				Type:  protocol.RespError,
				Error: err.Error(),
			}
		}
	}

	output := stdout.Bytes()
	if stderr.Len() > 0 {
		output = append(output, stderr.Bytes()...)
	}

	return &protocol.Response{
		Type:     protocol.RespExit,
		Data:     output,
		ExitCode: exitCode,
	}
}
