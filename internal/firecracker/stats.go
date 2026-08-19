package firecracker

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

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

// ProcStatSample is one read of /proc/<pid>/stat plus /proc/uptime: everything
// the daemon's stats handler needs from a single Executor round-trip.
type ProcStatSample struct {
	CPUUsec uint64  // cumulative user+sys CPU, microseconds (monotonic)
	MemMB   float64 // resident set size
	CPUPct  float64 // lifetime-average CPU%, the same figure `ps -o %cpu` reports
}

// userHZ is the kernel's clock-tick rate for the utime/stime fields in
// /proc/<pid>/stat. It is 100 on every Linux port Go builds for, and is fixed
// ABI for those fields regardless of CONFIG_HZ.
const userHZ = 100

// procPageSize is the page unit of /proc/<pid>/stat's rss field.
const procPageSize = 4096

// ProcessCumulativeStats reports a Firecracker process's cumulative CPU,
// resident memory, and lifetime-average CPU% from /proc.
//
// It replaces a pair of `ps` calls. `ps -o time=` prints [DD-]HH:MM:SS on
// Linux — whole seconds — but the daemon's only consumer deltas this value on
// a 2s poll to draw a CPU graph, so a VM at a steady 30% renders as
// 0%/50%/0%/50%. /proc/<pid>/stat counts in 10ms ticks: 100x the resolution,
// from a file read with no subprocess. Reading rss from the same sample also
// removes the second `ps` spawn per VM per poll.
//
// ParseCumulativePS is still the right tool on macOS, where BSD ps reports
// MM:SS.ff and there is no /proc — see internal/cli/stats.go's applevz path.
func ProcessCumulativeStats(ex Executor, pid int) (ProcStatSample, error) {
	out, err := ex.RunWithTimeout(
		fmt.Sprintf("cat /proc/%d/stat; echo '|'; cat /proc/uptime", pid), 10*time.Second)
	if err != nil {
		return ProcStatSample{}, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	s, err := ParseProcStat(out)
	if err != nil {
		return ProcStatSample{}, fmt.Errorf("parse /proc/%d/stat: %w", pid, err)
	}
	return s, nil
}

// ParseProcStat parses the combined `/proc/<pid>/stat | /proc/uptime` output
// produced by ProcessCumulativeStats.
//
// Field 2 (comm) is parenthesised and may itself contain spaces and
// parentheses, so the fixed-position fields are located relative to the LAST
// ')' rather than by splitting the whole line — the standard trap in
// /proc/<pid>/stat parsing.
func ParseProcStat(out string) (ProcStatSample, error) {
	statPart, uptimePart, ok := strings.Cut(out, "|")
	if !ok {
		return ProcStatSample{}, fmt.Errorf("unexpected output %q (want stat|uptime)", out)
	}

	statLine := strings.TrimSpace(statPart)
	close := strings.LastIndexByte(statLine, ')')
	if close < 0 {
		return ProcStatSample{}, fmt.Errorf("no comm field in %q", statLine)
	}
	// After the comm field the next token is field 3 (state), so field N sits
	// at index N-3 here.
	f := strings.Fields(statLine[close+1:])
	const (
		idxUtime     = 11 // field 14
		idxStime     = 12 // field 15
		idxStarttime = 19 // field 22
		idxRSS       = 21 // field 24
	)
	if len(f) <= idxRSS {
		return ProcStatSample{}, fmt.Errorf("want at least %d fields after comm, got %d", idxRSS+1, len(f))
	}

	utime, err := strconv.ParseUint(f[idxUtime], 10, 64)
	if err != nil {
		return ProcStatSample{}, fmt.Errorf("parse utime %q: %w", f[idxUtime], err)
	}
	stime, err := strconv.ParseUint(f[idxStime], 10, 64)
	if err != nil {
		return ProcStatSample{}, fmt.Errorf("parse stime %q: %w", f[idxStime], err)
	}
	starttime, err := strconv.ParseUint(f[idxStarttime], 10, 64)
	if err != nil {
		return ProcStatSample{}, fmt.Errorf("parse starttime %q: %w", f[idxStarttime], err)
	}
	rssPages, err := strconv.ParseUint(f[idxRSS], 10, 64)
	if err != nil {
		return ProcStatSample{}, fmt.Errorf("parse rss %q: %w", f[idxRSS], err)
	}

	uptimeFields := strings.Fields(strings.TrimSpace(uptimePart))
	if len(uptimeFields) == 0 {
		return ProcStatSample{}, fmt.Errorf("empty /proc/uptime")
	}
	uptime, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return ProcStatSample{}, fmt.Errorf("parse uptime %q: %w", uptimeFields[0], err)
	}

	totalTicks := utime + stime
	sample := ProcStatSample{
		CPUUsec: totalTicks * (1_000_000 / userHZ),
		MemMB:   float64(rssPages*procPageSize) / (1024 * 1024),
	}
	// Lifetime average, matching what `ps -o %cpu` reports. Guard the divisor:
	// a process sampled in the same tick it started has elapsed == 0.
	if elapsed := uptime - float64(starttime)/userHZ; elapsed > 0 {
		sample.CPUPct = 100 * (float64(totalTicks) / userHZ) / elapsed
	}
	return sample, nil
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
