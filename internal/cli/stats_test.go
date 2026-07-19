package cli

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/agentstep/mvm/internal/server"
)

func TestFilterStatsByNameKeepsOnlyRequested(t *testing.T) {
	all := []server.VMStats{
		{Name: "web", Status: "running"},
		{Name: "worker", Status: "running"},
		{Name: "db", Status: "stopped"},
	}
	got := filterStatsByName(all, []string{"worker", "db"})
	want := []server.VMStats{
		{Name: "worker", Status: "running"},
		{Name: "db", Status: "stopped"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterStatsByName() = %+v, want %+v", got, want)
	}
}

func TestFilterStatsByNameEmptyMeansAll(t *testing.T) {
	all := []server.VMStats{{Name: "web"}, {Name: "worker"}}
	got := filterStatsByName(all, nil)
	if !reflect.DeepEqual(got, all) {
		t.Errorf("filterStatsByName(nil) = %+v, want unchanged %+v", got, all)
	}
}

// Zero-row results must marshal to JSON "[]", not "null" — matching the
// daemon's own handleStatsVMs endpoint (internal/server/routes.go), which
// uses make([]VMStats, 0, ...). A nil slice would break JSON consumers that
// do e.g. `.length` on the output.
func TestFilterStatsByNameNoMatchProducesEmptyJSONArray(t *testing.T) {
	all := []server.VMStats{{Name: "web"}}
	filtered := filterStatsByName(all, []string{"does-not-exist"})
	if filtered == nil {
		t.Fatal("filterStatsByName should return a non-nil empty slice, got nil")
	}
	data, err := json.Marshal(filtered)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("json.Marshal(filtered) = %s, want []", data)
	}
}

func TestEmptyVMStatsSliceMarshalsToEmptyArray(t *testing.T) {
	all := []server.VMStats{}
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("json.Marshal(empty []server.VMStats{}) = %s, want []", data)
	}
}
