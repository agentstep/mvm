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
	"github.com/agentstep/mvm/internal/server"
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
	var all []server.VMStats

	// applevz: PID is the mvm-vz helper running natively on the macOS host
	// (see runStartAppleVZ's startResult.PID) — query it directly, no daemon
	// involved, matching every other applevz-vs-daemon split in this package
	// (list.go's localApplevzVMs, exec.go's runExecAppleVZ, etc.).
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	for _, vm := range localVMs {
		row := server.VMStats{Name: vm.Name, Backend: vm.Backend, PID: vm.PID, Status: vm.Status}
		if vm.Status == "running" && vm.PID > 0 {
			cpu, memMB, err := hostProcessStats(vm.PID)
			if err != nil {
				row.Error = err.Error()
			} else {
				row.CPUPct, row.MemMB = cpu, memMB
			}
		}
		all = append(all, row)
	}

	// Firecracker: best-effort daemon call, matching list.go's pattern — an
	// applevz-only host with no daemon running is not an error for `mvm
	// stats` either.
	if sc, err := requireDaemon(); err == nil {
		if stats, err := sc.StatsVMs(context.Background()); err == nil {
			all = append(all, stats...)
		}
	}

	all = filterStatsByName(all, names)

	if wantJSON {
		data, _ := json.MarshalIndent(all, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if len(all) == 0 {
		fmt.Println("No microVMs.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tBACKEND\tPID\tCPU%\tMEM(MiB)\tSTATUS")
	for _, row := range all {
		cpu, mem := "-", "-"
		if row.Error == "" && row.Status == "running" {
			cpu = fmt.Sprintf("%.1f", row.CPUPct)
			mem = fmt.Sprintf("%.0f", row.MemMB)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", row.Name, row.Backend, row.PID, cpu, mem, row.Status)
	}
	return w.Flush()
}

// filterStatsByName keeps only rows whose Name is in names; an empty names
// means "keep everything" (mvm stats with no positional args = all VMs).
func filterStatsByName(all []server.VMStats, names []string) []server.VMStats {
	if len(names) == 0 {
		return all
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var filtered []server.VMStats
	for _, row := range all {
		if want[row.Name] {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// hostProcessStats runs ps directly on the macOS host. Used for applevz
// VMs, whose PID is the mvm-vz helper process running natively on the host
// (see vm.StartResult.PID / runStartAppleVZ) — unlike the Firecracker
// backend's PID, which lives inside Lima's process namespace and must go
// through the daemon's Executor instead (see firecracker.ProcessStats,
// used by handleStatsVMs).
func hostProcessStats(pid int) (cpuPct, memMB float64, err error) {
	out, err := exec.Command("ps", "-o", "%cpu=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return firecracker.ParsePSOutput(string(out))
}
