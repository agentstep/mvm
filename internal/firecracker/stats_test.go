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

// === ParseCumulativePS / parseCPUTime ===

func TestParseCPUTime(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"00:12.50", 12_500_000},
		{"01:02", 62_000_000},
		{"01:02:03", 3_723_000_000},
		{"1-00:00:00", 86_400_000_000},
	}
	for _, c := range cases {
		got, err := parseCPUTime(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q: got %d want %d", c.in, got, c.want)
		}
	}
}

func TestParseCumulativePS(t *testing.T) {
	cpu, memMB, err := ParseCumulativePS("  00:12.50 102400\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cpu != 12_500_000 {
		t.Fatalf("cpu: got %d want 12500000", cpu)
	}
	if memMB != 100.0 {
		t.Fatalf("mem: got %v want 100", memMB)
	}
}

func TestParseCumulativePSBadFields(t *testing.T) {
	if _, _, err := ParseCumulativePS("only-one-field"); err == nil {
		t.Fatal("want error on malformed ps output")
	}
}

// === ProcessCumulativeCPU ===

type fixedExec struct{ out string }

func (f fixedExec) Run(string) (string, error)                           { return f.out, nil }
func (f fixedExec) RunWithTimeout(string, time.Duration) (string, error) { return f.out, nil }

func TestProcessCumulativeCPU(t *testing.T) {
	// `ps -o time=,rss=` → cumulative CPU time + rss.
	usec, err := ProcessCumulativeCPU(fixedExec{out: "  00:12.50 102400\n"}, 4242)
	if err != nil {
		t.Fatalf("ProcessCumulativeCPU: %v", err)
	}
	if usec != 12_500_000 {
		t.Errorf("cpuUsec = %d, want 12500000", usec)
	}
}
