package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
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

func TestInspectJSONIsOneElementArrayWithPlatform(t *testing.T) {
	resp := server.VMInspectResponse{
		VMResponse: server.VMResponse{
			Name: "web", Status: "running", GuestIP: "192.168.64.5",
			CreatedAt: time.Unix(1700000000, 0),
		},
		Spec: &state.VMSpec{Image: "nginx", Cpus: 2, MemoryMB: 512},
	}
	arr := []cfContainer{toCFContainer(resp.VMResponse, resp.Spec, true)}
	if len(arr) != 1 {
		t.Fatalf("inspect must wrap in a 1-element array, got %d", len(arr))
	}
	p := arr[0].Configuration.Platform
	if p == nil || p.OS != "linux" || p.Architecture != "arm64" {
		t.Fatalf("platform must be linux/arm64, got %+v", p)
	}
	if arr[0].Configuration.Resources.MemoryInBytes != 512*1024*1024 {
		t.Fatalf("memoryInBytes nesting wrong: %d", arr[0].Configuration.Resources.MemoryInBytes)
	}
}

// === runInspect: daemon-fallback branch ===

func TestRunInspectFallsBackToDaemon(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/vms/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{
			VMResponse: server.VMResponse{Name: r.PathValue("name"), Status: "running", Backend: "firecracker"},
			Spec:       &state.VMSpec{Cpus: 2},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	if err := runInspect(store, "daemon-vm", "json"); err != nil {
		t.Errorf("runInspect() = %v, want nil (a VM absent from local state must fall back to the daemon)", err)
	}
}
