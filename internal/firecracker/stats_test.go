package firecracker

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// === ParsePSOutput ===

func TestParsePSOutputParsesCPUAndRSS(t *testing.T) {
	cpu, memMB, err := ParsePSOutput(" 12.5 204800\n")
	if err != nil {
		t.Fatalf("ParsePSOutput: %v", err)
	}
	if cpu != 12.5 {
		t.Errorf("cpu = %v, want 12.5", cpu)
	}
	if memMB != 200 { // 204800 KiB / 1024 = 200 MiB
		t.Errorf("memMB = %v, want 200", memMB)
	}
}

func TestParsePSOutputRejectsMalformed(t *testing.T) {
	if _, _, err := ParsePSOutput("garbage"); err == nil {
		t.Fatal("ParsePSOutput(\"garbage\") = nil error, want error")
	}
	if _, _, err := ParsePSOutput(""); err == nil {
		t.Fatal("ParsePSOutput(\"\") = nil error, want error")
	}
	if _, _, err := ParsePSOutput("abc 123"); err == nil {
		t.Fatal("ParsePSOutput(\"abc 123\") = nil error, want error (bad cpu field)")
	}
}

// === ProcessStats ===

type stubStatsExecutor struct {
	out string
	err error
}

func (s *stubStatsExecutor) Run(command string) (string, error) { return s.out, s.err }
func (s *stubStatsExecutor) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	return s.out, s.err
}

func TestProcessStatsRunsPsWithPID(t *testing.T) {
	ex := &stubStatsExecutor{out: "3.0 51200\n"}
	cpu, memMB, err := ProcessStats(ex, 4242)
	if err != nil {
		t.Fatalf("ProcessStats: %v", err)
	}
	if cpu != 3.0 || memMB != 50 {
		t.Errorf("ProcessStats() = %v, %v, want 3.0, 50", cpu, memMB)
	}
}

func TestProcessStatsPropagatesExecutorError(t *testing.T) {
	ex := &stubStatsExecutor{err: fmt.Errorf("no such process")}
	if _, _, err := ProcessStats(ex, 1); err == nil {
		t.Fatal("ProcessStats() = nil error, want the executor's error propagated")
	} else if !strings.Contains(err.Error(), "no such process") {
		t.Errorf("error should wrap the underlying cause, got: %v", err)
	}
}
