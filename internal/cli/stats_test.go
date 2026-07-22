package cli

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/agentstep/mvm/internal/server"
)

func TestFilterSourcesByNameKeepsOnlyRequested(t *testing.T) {
	all := []cfStatSource{
		{Name: "web", Status: "running"},
		{Name: "worker", Status: "running"},
		{Name: "db", Status: "stopped"},
	}
	got := filterSourcesByName(all, []string{"worker", "db"})
	want := []cfStatSource{
		{Name: "worker", Status: "running"},
		{Name: "db", Status: "stopped"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterSourcesByName() = %+v, want %+v", got, want)
	}
}

func TestFilterSourcesByNameEmptyMeansAll(t *testing.T) {
	all := []cfStatSource{{Name: "web"}, {Name: "worker"}}
	got := filterSourcesByName(all, nil)
	if !reflect.DeepEqual(got, all) {
		t.Errorf("filterSourcesByName(nil) = %+v, want unchanged %+v", got, all)
	}
}

// Zero-row results must marshal to JSON "[]", not "null" — a nil slice would
// break JSON consumers that do e.g. `.length` on the output.
func TestFilterSourcesByNameNoMatchProducesEmptyJSONArray(t *testing.T) {
	all := []cfStatSource{{Name: "web"}}
	filtered := filterSourcesByName(all, []string{"does-not-exist"})
	if filtered == nil {
		t.Fatal("filterSourcesByName should return a non-nil empty slice, got nil")
	}
	data, err := json.Marshal(toCFStats(filtered))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("json.Marshal(filtered) = %s, want []", data)
	}
}

func TestStatsJSONShape(t *testing.T) {
	src := []cfStatSource{
		{Name: "web", CPUUsageUsec: 12_500_000, MemoryUsageBytes: 104857600, MemoryLimitBytes: 536870912, NumProcesses: 1},
		{Name: "db", MemoryLimitBytes: 268435456, NumProcesses: 1},
	}
	got := filterSourcesByName(src, []string{"web"})
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("name filter: got %+v", got)
	}
	out, _ := json.MarshalIndent(toCFStats(got), "", "  ")
	want := `[
  {
    "id": "web",
    "cpuUsageUsec": 12500000,
    "memoryUsageBytes": 104857600,
    "memoryLimitBytes": 536870912,
    "numProcesses": 1
  }
]`
	if string(out) != want {
		t.Fatalf("stats json:\n got:\n%s\nwant:\n%s", out, want)
	}
}

func TestStatsFCSourceCarriesCumulativeCPU(t *testing.T) {
	// A daemon VMStats with a cumulative µs value must flow into cfStatSource
	// and out as cpuUsageUsec (not dropped to 0 as in Slice 1).
	vs := server.VMStats{Name: "web", Backend: "firecracker", PID: 1, Status: "running", CPUUsageUsec: 9_000_000, MemMB: 100}
	src := cfStatSource{
		Name: vs.Name, Backend: vs.Backend, PID: vs.PID, Status: vs.Status,
		CPUUsageUsec:     vs.CPUUsageUsec,
		MemoryUsageBytes: uint64(vs.MemMB * 1024 * 1024),
		NumProcesses:     1,
	}
	out := toCFStats([]cfStatSource{src})
	if out[0].CPUUsageUsec != 9_000_000 {
		t.Errorf("cpuUsageUsec = %d, want 9000000", out[0].CPUUsageUsec)
	}
}
