package firecracker

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubStatsExecutor is a canned Executor, with an error path so failure
// propagation can be asserted without a real host.
type stubStatsExecutor struct {
	out string
	err error
}

func (s *stubStatsExecutor) Run(command string) (string, error) { return s.out, s.err }
func (s *stubStatsExecutor) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	return s.out, s.err
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

// === ProcessCumulativeStats / ParseProcStat ===

// procStatLine builds a synthetic /proc/<pid>/stat line: pid, the
// parenthesised comm, then fields 3..24 with the ones ParseProcStat reads set
// and everything else zeroed.
func procStatLine(comm string, utime, stime, starttime, rssPages uint64) string {
	f := make([]string, 22) // fields 3..24, so field N is at index N-3
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S" // field 3: state
	set := func(field int, v uint64) { f[field-3] = strconv.FormatUint(v, 10) }
	set(14, utime)
	set(15, stime)
	set(22, starttime)
	set(24, rssPages)
	return "4242 (" + comm + ") " + strings.Join(f, " ")
}

func TestParseProcStat(t *testing.T) {
	// 1500 + 1500 ticks at 100Hz = 30s cumulative CPU.
	// 25600 pages * 4096 = 100 MiB rss.
	out := procStatLine("firecracker", 1500, 1500, 0, 25600) + "\n|\n1000.0 900.0\n"
	s, err := ParseProcStat(out)
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	if s.CPUUsec != 30_000_000 {
		t.Errorf("CPUUsec = %d, want 30000000", s.CPUUsec)
	}
	if s.MemMB != 100 {
		t.Errorf("MemMB = %v, want 100", s.MemMB)
	}
	// 30s CPU over 1000s elapsed = 3%.
	if s.CPUPct < 2.99 || s.CPUPct > 3.01 {
		t.Errorf("CPUPct = %v, want ~3", s.CPUPct)
	}
}

// TestParseProcStatResolutionBeatsPS is the regression guard on why this
// replaced `ps -o time=`. procps prints whole seconds on Linux, so a delta
// between two 2s-apart samples could only ever be 0 or 1000000µs. Ticks are
// 10ms, so a sub-second delta survives.
func TestParseProcStatResolutionBeatsPS(t *testing.T) {
	first, err := ParseProcStat(procStatLine("firecracker", 100, 0, 0, 0) + "|1000.0 900.0")
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	// +7 ticks = 70ms, a value `ps -o time=` would round away entirely.
	second, err := ParseProcStat(procStatLine("firecracker", 107, 0, 0, 0) + "|1002.0 900.0")
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	if got := second.CPUUsec - first.CPUUsec; got != 70_000 {
		t.Errorf("delta = %dµs, want 70000 (sub-second resolution)", got)
	}
}

// TestParseProcStatCommWithSpaces pins the standard /proc/<pid>/stat trap:
// field 2 is unquoted and may contain spaces and parentheses, so fixed-offset
// fields must be located from the LAST ')' rather than by splitting the line.
func TestParseProcStatCommWithSpaces(t *testing.T) {
	s, err := ParseProcStat(procStatLine("weird (name) here", 1000, 0, 0, 25600) + "|1000.0 900.0")
	if err != nil {
		t.Fatalf("ParseProcStat with spaced comm: %v", err)
	}
	if s.CPUUsec != 10_000_000 {
		t.Errorf("CPUUsec = %d, want 10000000 — comm parsing shifted the fields", s.CPUUsec)
	}
}

func TestParseProcStatMalformed(t *testing.T) {
	for _, in := range []string{
		"no pipe here",
		"4242 (fc) S 1 2 3|1000.0",           // too few fields
		"no-paren 1 2 3|1000.0",              // no comm
		procStatLine("fc", 1, 1, 0, 1) + "|", // empty uptime
	} {
		if _, err := ParseProcStat(in); err == nil {
			t.Errorf("ParseProcStat(%q) should have errored", in)
		}
	}
}

func TestProcessCumulativeStats(t *testing.T) {
	out := procStatLine("firecracker", 1500, 1500, 0, 25600) + "|1000.0 900.0"
	s, err := ProcessCumulativeStats(&stubStatsExecutor{out: out}, 4242)
	if err != nil {
		t.Fatalf("ProcessCumulativeStats: %v", err)
	}
	if s.CPUUsec != 30_000_000 {
		t.Errorf("CPUUsec = %d, want 30000000", s.CPUUsec)
	}
	if s.MemMB != 100 {
		t.Errorf("MemMB = %v, want 100", s.MemMB)
	}
}

func TestProcessCumulativeStatsPropagatesExecutorError(t *testing.T) {
	ex := &stubStatsExecutor{err: fmt.Errorf("no such process")}
	if _, err := ProcessCumulativeStats(ex, 1); err == nil {
		t.Fatal("ProcessCumulativeStats() = nil error, want the executor's error propagated")
	} else if !strings.Contains(err.Error(), "no such process") {
		t.Errorf("error should wrap the underlying cause, got: %v", err)
	}
}
