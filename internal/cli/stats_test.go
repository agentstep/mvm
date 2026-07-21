package cli

import (
	"encoding/json"
	"reflect"
	"testing"
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
