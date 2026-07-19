package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// The response-shaping logic itself (no internal fields leak, Spec carried
// through) now lives in server.InspectResponseFromVM and is tested there —
// this only checks that runInspect's applevz branch reaches it successfully
// without requiring a daemon.
func TestRunInspectAppleVZDoesNotRequireDaemon(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.AddVM(&state.VM{
		Name:      "web",
		Status:    "running",
		Backend:   "applevz",
		CreatedAt: time.Now(),
		Spec:      &state.VMSpec{Cpus: 4},
	})

	if err := runInspect(store, "web", "json"); err != nil {
		t.Errorf("runInspect() = %v, want nil (applevz VMs must not require a daemon)", err)
	}
}
