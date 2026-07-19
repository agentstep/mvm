package cli

import (
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
