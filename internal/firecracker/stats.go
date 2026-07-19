package firecracker

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePSOutput parses the output of `ps -o %cpu=,rss= -p <pid>` (two
// whitespace-separated numeric fields, no header — the trailing "=" on each
// -o key suppresses ps's column header). Shared by ProcessStats (Firecracker,
// run inside Lima via an Executor) and the CLI's own host-local ps call for
// applevz PIDs (internal/cli/stats.go's hostProcessStats), so the parsing
// logic — and its test coverage — isn't duplicated between the two
// transports.
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
