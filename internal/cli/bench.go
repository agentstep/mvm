package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

// newBenchCmd measures each boot path on the local node by driving throwaway
// VMs in-process and reporting p50/p90/p95 — separately per path, so a warm
// number is never published as a cold-boot headline. Measuring in-process
// (via runStartAppleVZ's returned BootResult) sidesteps the subprocess
// pipe-EOF trap that makes `mvm start` look like it hangs from a script.
func newBenchCmd(store *state.Store) *cobra.Command {
	var (
		samples int
		jsonOut bool
		keep    bool
	)
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Benchmark boot paths (cold_boot / snapshot_restore) with p50/p90/p95",
		Long: `Measure each VM boot path on this machine and report latency percentiles.

Drives a throwaway VM ` + benchVMName + ` through each path; every sample is
verified agent-ready before it counts. Apple VZ backend only.

  mvm bench                  # 5 samples per path, human table
  mvm bench --samples 10     # more samples
  mvm bench --json           # machine-readable`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBench(store, samples, jsonOut, keep)
		},
	}
	cmd.Flags().IntVar(&samples, "samples", 5, "samples per boot path")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a table")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the bench VM at the end (default: clean up)")
	return cmd
}

const benchVMName = "mvm-bench"

// pathStats holds the percentile summary for one boot path.
type pathStats struct {
	Path    BootPath  `json:"path"`
	Samples int       `json:"samples"`
	Failed  int       `json:"failed"`
	P50Ms   float64   `json:"p50_ms"`
	P90Ms   float64   `json:"p90_ms"`
	P95Ms   float64   `json:"p95_ms"`
	MinMs   float64   `json:"min_ms"`
	MaxMs   float64   `json:"max_ms"`
	RawMs   []float64 `json:"raw_ms"`
}

func runBench(store *state.Store, samples int, jsonOut, keep bool) error {
	if store.GetBackend() != "applevz" {
		return fmt.Errorf("mvm bench currently supports the applevz backend only")
	}
	if samples < 1 {
		return fmt.Errorf("--samples must be >= 1")
	}

	// The lifecycle helpers (stop, snapshot-create) print to stdout. In JSON
	// mode stdout must carry only the final result, so divert it to /dev/null
	// for the duration of the run and restore it to emit the JSON.
	realStdout := os.Stdout
	if jsonOut {
		if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
			os.Stdout = devnull
			defer func() { os.Stdout = realStdout; devnull.Close() }()
		}
	}

	benchClean(store) // start from a known-clean slate

	var results []pathStats

	// --- cold_boot: full teardown between each sample ---
	cold := pathStats{Path: BootCold}
	for i := 0; i < samples; i++ {
		benchClean(store)
		ms, ok := benchOneStart(store)
		if ok {
			cold.RawMs = append(cold.RawMs, ms)
		} else {
			cold.Failed++
		}
		benchProgress(jsonOut, "cold_boot %d/%d\n", i+1, samples)
	}
	results = append(results, summarize(cold))

	// --- snapshot_restore: checkpoint once, then stop+restore repeatedly ---
	restore := pathStats{Path: BootRestore}
	benchClean(store)
	if _, ok := benchOneStart(store); ok {
		if err := runSnapshotCreate(context.Background(), store, benchVMName, ""); err != nil {
			benchProgress(jsonOut, "checkpoint failed: %v\n", err)
		} else {
			for i := 0; i < samples; i++ {
				_ = runStop(store, benchVMName, false)
				time.Sleep(500 * time.Millisecond) // let the helper release the disk lock
				ms, ok := benchOneStart(store)
				if ok {
					restore.RawMs = append(restore.RawMs, ms)
				} else {
					restore.Failed++
				}
				benchProgress(jsonOut, "snapshot_restore %d/%d\n", i+1, samples)
			}
		}
	}
	results = append(results, summarize(restore))

	if !keep {
		benchClean(store)
	}

	if jsonOut {
		return json.NewEncoder(realStdout).Encode(results)
	}
	printBenchTable(results)
	return nil
}

// benchOneStart starts the bench VM and returns its total boot time in ms,
// counting it only if the start succeeded and the agent answered.
func benchOneStart(store *state.Store) (float64, bool) {
	res, err := runStartAppleVZ(store, benchVMName, false, nil, "open", 0, 0, nil, outQuiet, nil)
	if err != nil || res == nil || !res.AgentReady {
		return 0, false
	}
	return res.TotalMs, true
}

// benchClean tears down the bench VM and removes its on-disk artifacts so the
// next cold boot is genuinely cold. `mvm delete` needs the daemon for some
// backends; for applevz we stop + purge the record + rm the VM dir directly.
func benchClean(store *state.Store) {
	_ = runStop(store, benchVMName, true)
	time.Sleep(300 * time.Millisecond)
	store.RemoveVM(benchVMName)
	home, _ := os.UserHomeDir()
	os.RemoveAll(filepath.Join(home, ".mvm", "vms", benchVMName))
	os.Remove(filepath.Join(home, ".mvm", "run", "vz-"+benchVMName+".sock"))
	time.Sleep(300 * time.Millisecond)
}

func benchProgress(jsonOut bool, format string, a ...any) {
	// Progress to stderr; stdout is reserved for the final table/JSON.
	fmt.Fprintf(os.Stderr, "  "+format, a...)
}

func summarize(p pathStats) pathStats {
	p.Samples = len(p.RawMs)
	if p.Samples == 0 {
		return p
	}
	sorted := append([]float64(nil), p.RawMs...)
	sort.Float64s(sorted)
	p.MinMs = sorted[0]
	p.MaxMs = sorted[len(sorted)-1]
	p.P50Ms = percentile(sorted, 50)
	p.P90Ms = percentile(sorted, 90)
	p.P95Ms = percentile(sorted, 95)
	return p
}

// percentile uses nearest-rank on an already-sorted slice.
func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func printBenchTable(results []pathStats) {
	fmt.Printf("\n%-18s %7s %7s %7s %7s %7s %8s\n", "boot_path", "n", "p50", "p90", "p95", "min", "failed")
	fmt.Println("  " + "------------------------------------------------------------------")
	for _, r := range results {
		if r.Samples == 0 {
			fmt.Printf("%-18s %7s %7s %7s %7s %7s %8d\n", r.Path, "-", "-", "-", "-", "-", r.Failed)
			continue
		}
		fmt.Printf("%-18s %7d %6.0fms %6.0fms %6.0fms %6.0fms %8d\n",
			r.Path, r.Samples, r.P50Ms, r.P90Ms, r.P95Ms, r.MinMs, r.Failed)
	}
}
