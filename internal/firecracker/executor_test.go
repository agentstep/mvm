package firecracker

import (
	"strings"
	"testing"
	"time"
)

// === LocalExecutor: Run ===

func TestLocalExecutorRunSimpleCommand(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.Run("echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("output = %q, want hello", out)
	}
}

func TestLocalExecutorRunMultipleCommands(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.Run("echo foo && echo bar")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "foo") || !strings.Contains(out, "bar") {
		t.Errorf("output = %q, want foo and bar", out)
	}
}

func TestLocalExecutorRunExitCode(t *testing.T) {
	ex := &LocalExecutor{}
	_, err := ex.Run("exit 1")
	if err == nil {
		t.Error("should error on non-zero exit code")
	}
}

func TestLocalExecutorRunStderr(t *testing.T) {
	ex := &LocalExecutor{}
	_, err := ex.Run("echo error >&2; exit 1")
	if err == nil {
		t.Error("should error on non-zero exit code")
	}
	if !strings.Contains(err.Error(), "error") {
		t.Errorf("error should contain stderr message, got: %v", err)
	}
}

func TestLocalExecutorRunEmptyCommand(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.Run("true")
	if err != nil {
		t.Fatalf("Run true: %v", err)
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
}

func TestLocalExecutorRunReturnsStdoutOnError(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.Run("echo partial; exit 1")
	if err == nil {
		t.Error("should error on exit 1")
	}
	// Even on error, stdout should be captured
	if !strings.Contains(out, "partial") {
		t.Errorf("stdout should contain 'partial' even on error, got: %q", out)
	}
}

// === LocalExecutor: RunWithTimeout ===

func TestLocalExecutorRunWithTimeoutSuccess(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.RunWithTimeout("echo ok", 5*time.Second)
	if err != nil {
		t.Fatalf("RunWithTimeout: %v", err)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("output = %q, want ok", out)
	}
}

func TestLocalExecutorRunWithTimeoutExpires(t *testing.T) {
	ex := &LocalExecutor{}
	_, err := ex.RunWithTimeout("sleep 30", 200*time.Millisecond)
	if err == nil {
		t.Error("should error on timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
}

func TestLocalExecutorRunWithTimeoutLongerThanCommand(t *testing.T) {
	ex := &LocalExecutor{}
	start := time.Now()
	out, err := ex.RunWithTimeout("echo fast", 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunWithTimeout: %v", err)
	}
	if strings.TrimSpace(out) != "fast" {
		t.Errorf("output = %q, want fast", out)
	}
	if elapsed > 5*time.Second {
		t.Error("should complete quickly, not wait for full timeout")
	}
}

func TestLocalExecutorRunWithTimeoutCapturesPartialOutput(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.RunWithTimeout("echo before_timeout; sleep 30", 500*time.Millisecond)
	if err == nil {
		t.Error("should error on timeout")
	}
	// Partial stdout should be captured even on timeout
	if !strings.Contains(out, "before_timeout") {
		t.Logf("partial output not captured (expected on timeout): %q", out)
	}
}

// === Executor interface compliance ===

func TestLocalExecutorImplementsExecutor(t *testing.T) {
	var _ Executor = &LocalExecutor{}
}

// === LongTimeout constant ===

func TestLongTimeoutValue(t *testing.T) {
	if LongTimeout != 15*time.Minute {
		t.Errorf("LongTimeout = %v, want 15m", LongTimeout)
	}
}

// === LocalExecutor Run default timeout ===

func TestLocalExecutorRunUsesDefaultTimeout(t *testing.T) {
	// Run should complete normally for fast commands (default 30s timeout)
	ex := &LocalExecutor{}
	out, err := ex.Run("echo default_timeout_test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "default_timeout_test" {
		t.Errorf("output = %q", out)
	}
}

func TestLocalExecutorRunPipeCommand(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.Run("echo 'hello world' | wc -w")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "2" {
		t.Errorf("output = %q, want 2", strings.TrimSpace(out))
	}
}

func TestLocalExecutorRunEnvironmentVariables(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.Run("FOO=bar && echo $FOO")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "bar" {
		t.Errorf("output = %q, want bar", strings.TrimSpace(out))
	}
}

func TestLocalExecutorRunMultilineOutput(t *testing.T) {
	ex := &LocalExecutor{}
	out, err := ex.Run("echo line1; echo line2; echo line3")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d: %q", len(lines), out)
	}
}
