package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"text/tabwriter"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newStatsCmd(store *state.Store) *cobra.Command {
	var (
		format   string
		noStream bool
	)

	cmd := &cobra.Command{
		Use:   "stats [name...]",
		Short: "Show live resource usage for running microVMs",
		Long: `Show a point-in-time snapshot of CPU and memory usage for running microVMs.

  mvm stats                  # all running VMs
  mvm stats my-vm             # a single VM
  mvm stats --format json     # machine-readable

v1 is point-in-time only (like "docker stats --no-stream"). Continuous
streaming is a follow-up; --no-stream is accepted for forward-compatibility
but is currently the only supported mode — omitting it behaves identically.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := resolveFormat(format, false)
			if err != nil {
				return err
			}
			return runStats(store, args, wantJSON)
		},
	}

	cmd.Flags().StringVar(&format, "format", "", "output format: table (default) or json")
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "disable streaming (default and only supported mode in v1)")

	return cmd
}

func runStats(store *state.Store, names []string, wantJSON bool) error {
	specs := specsByName(store)
	sources := []cfStatSource{}

	// applevz: PID is the mvm-vz helper running natively on the macOS host
	// (see runStartAppleVZ's startResult.PID) — query it directly, no daemon
	// involved, matching every other applevz-vs-daemon split in this package
	// (list.go's localApplevzVMs, exec.go's runExecAppleVZ, etc.).
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	for _, vm := range localVMs {
		src := cfStatSource{
			Name: vm.Name, Backend: vm.Backend, PID: vm.PID, Status: vm.Status,
			MemoryLimitBytes: memLimitBytes(specs[vm.Name]), NumProcesses: 1,
		}
		if vm.Status == "running" && vm.PID > 0 {
			if cpuUsec, memMB, err := hostCumulativeStats(vm.PID); err == nil {
				src.CPUUsageUsec = cpuUsec
				src.MemoryUsageBytes = uint64(memMB * 1024 * 1024)
			}
		}
		sources = append(sources, src)
	}

	// Firecracker: best-effort daemon call, matching list.go's pattern — an
	// applevz-only host with no daemon running is not an error for `mvm
	// stats` either. The frozen VMStats wire contract carries only an
	// instantaneous %cpu, so cpuUsageUsec stays 0 for the daemon path until
	// an additive cumulative-CPU endpoint lands (Slice 3).
	if sc, err := requireDaemon(); err == nil {
		if stats, err := sc.StatsVMs(context.Background()); err == nil {
			for _, s := range stats {
				sources = append(sources, cfStatSource{
					Name: s.Name, Backend: s.Backend, PID: s.PID, Status: s.Status,
					CPUUsageUsec:     s.CPUUsageUsec,
					MemoryUsageBytes: uint64(s.MemMB * 1024 * 1024),
					MemoryLimitBytes: memLimitBytes(specs[s.Name]), NumProcesses: 1,
				})
			}
		}
	}

	sources = filterSourcesByName(sources, names)

	if wantJSON {
		data, _ := json.MarshalIndent(toCFStats(sources), "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if len(sources) == 0 {
		fmt.Println("No microVMs.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tBACKEND\tPID\tCPU(s)\tMEM(MiB)\tPROCS\tSTATUS")
	for _, s := range sources {
		cpu, mem := "-", "-"
		if s.Status == "running" {
			cpu = fmt.Sprintf("%.1f", float64(s.CPUUsageUsec)/1e6)
			mem = fmt.Sprintf("%.0f", float64(s.MemoryUsageBytes)/(1024*1024))
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%d\t%s\n", s.Name, s.Backend, s.PID, cpu, mem, s.NumProcesses, s.Status)
	}
	return w.Flush()
}

// hostCumulativeStats runs ps directly on the macOS host. Used for applevz
// VMs, whose PID is the mvm-vz helper process running natively on the host
// (see vm.StartResult.PID / runStartAppleVZ) — unlike the Firecracker
// backend's PID, which lives inside Lima's process namespace and must go
// through the daemon's Executor instead. Reports CUMULATIVE CPU microseconds
// (monotonic), the fidelity cfStats.cpuUsageUsec needs.
func hostCumulativeStats(pid int) (cpuUsec uint64, memMB float64, err error) {
	out, err := exec.Command("ps", "-o", "time=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return firecracker.ParseCumulativePS(string(out))
}

// memLimitBytes reports the configured memory ceiling for a VM in bytes, or 0
// when the spec is unknown (persisted spec missing).
func memLimitBytes(spec *state.VMSpec) uint64 {
	if spec == nil {
		return 0
	}
	return uint64(spec.MemoryMB) * 1024 * 1024
}

// filterSourcesByName keeps only sources whose Name is in names; an empty names
// means "keep everything" (mvm stats with no positional args = all VMs). Always
// returns a non-nil slice so the empty case marshals to JSON "[]", not "null".
func filterSourcesByName(all []cfStatSource, names []string) []cfStatSource {
	if len(names) == 0 {
		return all
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	filtered := []cfStatSource{}
	for _, s := range all {
		if want[s.Name] {
			filtered = append(filtered, s)
		}
	}
	return filtered
}
