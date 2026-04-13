package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// LongTimeout is the default for long-running operations.
const LongTimeout = 15 * time.Minute

// Executor runs shell commands on the Firecracker host (the Lima VM).
// lima.Client implements this via Run/RunWithTimeout.
// LocalExecutor implements this for daemon-inside-Lima (direct exec).
type Executor interface {
	Run(command string) (string, error)
	RunWithTimeout(command string, timeout time.Duration) (string, error)
}

// LocalExecutor runs commands directly (for daemon running inside Lima).
type LocalExecutor struct{}

func (e *LocalExecutor) Run(command string) (string, error) {
	return e.RunWithTimeout(command, 30*time.Second)
}

func (e *LocalExecutor) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), fmt.Errorf("command timed out after %v", timeout)
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}
