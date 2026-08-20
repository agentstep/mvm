package firecracker

import "testing"

// Real /proc/<pid>/stat captured from the mvm-daemon running inside Lima,
// paired with the real /proc/uptime from the same moment. Guards the field
// offsets against actual kernel output rather than synthetic data.
func TestParseProcStatRealKernelOutput(t *testing.T) {
	const real = "1885 (mvm-daemon) S 1 1885 1885 0 -1 4194560 1196 0 0 0 0 0 0 0 20 0 9 0 11308 1296891904 2142 18446744073709551615 65536 3571476 281474044391120 0 0 0 1002060288 0 2143420159 0 0 0 17 0 0 0 0 0 0 7798784 8180800 974344192 281474044395115 281474044395148 281474044395148 281474044395492 0|241.38 955.50"

	s, err := ParseProcStat(real)
	if err != nil {
		t.Fatalf("ParseProcStat on real kernel output: %v", err)
	}
	// utime=0 stime=0 for a mostly-idle daemon → 0 cumulative CPU.
	if s.CPUUsec != 0 {
		t.Errorf("CPUUsec = %d, want 0 (both utime and stime are 0 here)", s.CPUUsec)
	}
	// rss field (24) is 2142 pages * 4096 = ~8.4 MB.
	if s.MemMB < 8 || s.MemMB > 9 {
		t.Errorf("MemMB = %v, want ~8.4 (2142 pages)", s.MemMB)
	}
	// starttime=11308 ticks = 113s; uptime 241.38s → ~128s elapsed, 0 CPU → 0%.
	if s.CPUPct != 0 {
		t.Errorf("CPUPct = %v, want 0", s.CPUPct)
	}
}
