package firecracker

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParsePSOutput parses the output of `ps -o %cpu=,rss= -p <pid>` (two
// whitespace-separated numeric fields, no header — the trailing "=" on each
// -o key suppresses ps's column header). Used by ProcessStats (Firecracker,
// run inside Lima via an Executor). Reports an instantaneous %cpu; for the
// cumulative-CPU-microseconds fidelity cfStats needs, see ParseCumulativePS.
func ParsePSOutput(out string) (cpuPct, memMB float64, err error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ps output %q (want 2 fields: %%cpu rss)", out)
	}
	cpuPct, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %%cpu %q: %w", fields[0], err)
	}
	rssKB, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse rss %q: %w", fields[1], err)
	}
	return cpuPct, rssKB / 1024.0, nil
}

// ProcessStats reports point-in-time CPU% and resident memory (MiB) for pid,
// which must be a PID inside ex's namespace — for the Firecracker backend
// that's Lima's process namespace, not the macOS host's (see process.go's
// parsePID / Start, which reports the Firecracker binary's PID as observed
// by the same Executor). This is a snapshot, not a rate: %cpu as ps reports
// it is a lifetime-average, not "usage over the last second" — a true
// live/streaming view would need to poll and diff /proc/<pid>/stat, tracked
// as a follow-up (see docs/superpowers/plans/2026-07-19-container-ergonomics.md Task 6).
func ProcessStats(ex Executor, pid int) (cpuPct, memMB float64, err error) {
	out, err := ex.Run(fmt.Sprintf("ps -o %%cpu=,rss= -p %d", pid))
	if err != nil {
		return 0, 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return ParsePSOutput(out)
}

// ParseCumulativePS parses `ps -o time=,rss= -p <pid>` — two fields: cumulative
// CPU time [[DD-]HH:]MM:SS[.ff] and resident memory in KiB. Returns cumulative
// CPU in microseconds (monotonic) and memory in MiB. Portable across BSD ps
// (macOS/applevz) and Linux ps (Lima/firecracker) — both spell cumulative CPU
// as the `time` keyword.
func ParseCumulativePS(out string) (cpuUsec uint64, memMB float64, err error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ps output %q (want 2 fields: time rss)", out)
	}
	cpuUsec, err = parseCPUTime(fields[0])
	if err != nil {
		return 0, 0, err
	}
	rssKB, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse rss %q: %w", fields[1], err)
	}
	return cpuUsec, rssKB / 1024.0, nil
}

// ProcessCumulativeCPU returns a process's CUMULATIVE CPU time in microseconds
// (monotonic — the value cfStats.cpuUsageUsec needs, which the dashboard deltas
// across samples). It reads `ps -o time=,rss=` and parses via ParseCumulativePS,
// discarding the memory field. Used by the daemon's stats handler so the
// Firecracker path reports real cumulative CPU instead of 0.
func ProcessCumulativeCPU(ex Executor, pid int) (uint64, error) {
	out, err := ex.RunWithTimeout(fmt.Sprintf("ps -o time=,rss= -p %d", pid), 10*time.Second)
	if err != nil {
		return 0, err
	}
	usec, _, err := ParseCumulativePS(out)
	return usec, err
}

// parseCPUTime converts a ps `time` field — [[DD-]HH:]MM:SS[.ff] — to microseconds.
func parseCPUTime(s string) (uint64, error) {
	days := 0
	rest := s
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		d, err := strconv.Atoi(rest[:i])
		if err != nil {
			return 0, fmt.Errorf("parse cpu days %q: %w", s, err)
		}
		days = d
		rest = rest[i+1:]
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("parse cpu time %q: want [[DD-]HH:]MM:SS", s)
	}
	var hours, mins int
	var secs float64
	var err error
	if len(parts) == 3 {
		if hours, err = strconv.Atoi(parts[0]); err != nil {
			return 0, fmt.Errorf("parse cpu hours %q: %w", s, err)
		}
		parts = parts[1:]
	}
	if mins, err = strconv.Atoi(parts[0]); err != nil {
		return 0, fmt.Errorf("parse cpu minutes %q: %w", s, err)
	}
	if secs, err = strconv.ParseFloat(parts[1], 64); err != nil {
		return 0, fmt.Errorf("parse cpu seconds %q: %w", s, err)
	}
	total := float64(days)*86400 + float64(hours)*3600 + float64(mins)*60 + secs
	return uint64(total * 1e6), nil
}
