package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

// === waitForReady ===

func TestWaitForReadySucceedsImmediately(t *testing.T) {
	calls := 0
	err := waitForReady(time.Second, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("waitForReady() = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("probe called %d times, want 1", calls)
	}
}

func TestWaitForReadyRetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := waitForReady(2*time.Second, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("not ready yet")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("waitForReady() = %v, want nil", err)
	}
	if calls != 3 {
		t.Errorf("probe called %d times, want 3", calls)
	}
}

func TestWaitForReadyTimesOut(t *testing.T) {
	calls := 0
	err := waitForReady(250*time.Millisecond, func() error {
		calls++
		return fmt.Errorf("never ready")
	})
	if err == nil {
		t.Fatal("waitForReady() = nil, want a timeout error")
	}
	if calls < 1 {
		t.Errorf("probe called %d times, want at least 1", calls)
	}
}

func TestWaitForReadyBackoffCapsAt2s(t *testing.T) {
	var gaps []time.Duration
	last := time.Now()
	calls := 0
	waitForReady(9*time.Second, func() error {
		now := time.Now()
		if calls > 0 {
			gaps = append(gaps, now.Sub(last))
		}
		last = now
		calls++
		if calls >= 6 {
			return nil
		}
		return fmt.Errorf("not ready yet")
	})
	for i, gap := range gaps {
		if gap > 2500*time.Millisecond {
			t.Errorf("gap[%d] = %v, want capped at ~2s (with scheduling slack)", i, gap)
		}
	}
}

// === resolveImage ===

func TestResolveImageMapsBaseToDefault(t *testing.T) {
	if got := resolveImage("base"); got != "" {
		t.Errorf(`resolveImage("base") = %q, want "" (implicit default)`, got)
	}
}

func TestResolveImagePassesThroughCustomImages(t *testing.T) {
	if got := resolveImage("my-image"); got != "my-image" {
		t.Errorf(`resolveImage("my-image") = %q, want unchanged`, got)
	}
}

// === resolveRunName ===

func TestResolveRunNameUsesFlagName(t *testing.T) {
	if got := resolveRunName("mybox", map[string]bool{}); got != "mybox" {
		t.Errorf("resolveRunName = %q, want mybox", got)
	}
}

func TestResolveRunNameGeneratesWhenEmpty(t *testing.T) {
	if got := resolveRunName("", map[string]bool{}); got == "" {
		t.Error("resolveRunName(\"\") = empty, want a generated name")
	}
}

// === existingVMNames ===

func TestExistingVMNamesIncludesLocalApplevzVMs(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.AddVM(&state.VM{Name: "web", Backend: "applevz", Status: "running", CreatedAt: time.Now()})
	store.AddVM(&state.VM{Name: "worker", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()})

	names, err := existingVMNames(store)
	if err != nil {
		t.Fatalf("existingVMNames: %v", err)
	}
	if !names["web"] || !names["worker"] {
		t.Errorf("names = %v, want web and worker present", names)
	}
}

func TestExistingVMNamesMergesDaemonVMs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]server.VMResponse{
			{Name: "daemon-vm-1", Status: "running", Backend: "firecracker"},
			{Name: "daemon-vm-2", Status: "stopped", Backend: "firecracker"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.AddVM(&state.VM{Name: "local-applevz", Backend: "applevz", Status: "running", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	names, err := existingVMNames(store)
	if err != nil {
		t.Fatalf("existingVMNames: %v", err)
	}
	for _, want := range []string{"local-applevz", "daemon-vm-1", "daemon-vm-2"} {
		if !names[want] {
			t.Errorf("names = %v, want %q present (merged from local applevz state + fake daemon)", names, want)
		}
	}
}

// === runRun: applevz custom-image guard ===

func TestRunRunNoLongerHardBlocksCustomImageOnAppleVZ(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")

	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "", false)
	// Custom images on applevz are no longer a hard-blocked feature (Phase 2
	// of docs/superpowers/plans/2026-07-19-backend-parity.md). Whatever this
	// machine's local daemon/cache state produces, the error — if any — must
	// come from actual image resolution failing, never from the old blanket
	// rejection.
	if err != nil && strings.Contains(err.Error(), "not supported on the Apple VZ backend") {
		t.Fatalf("runRun() = %v, want the old blanket applevz --image rejection to be gone", err)
	}
}

// === runRun: backend-load-error must surface, not silently default ===

func TestRunRunSurfacesBackendLoadError(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := os.WriteFile(store.Path(), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "", false)
	if err == nil {
		t.Fatal("runRun() = nil, want the corrupt-state load error surfaced (not silently defaulting to firecracker and booting the wrong rootfs)")
	}
}

// === resolveRmFlag ===

func TestResolveRmFlagDetachErrors(t *testing.T) {
	_, err := resolveRmFlag(true, true)
	if err == nil || !strings.Contains(err.Error(), "--rm requires a foreground command") {
		t.Fatalf("resolveRmFlag(true, true) err = %v, want the foreground-requirement error", err)
	}
}

func TestResolveRmFlagForegroundDeletes(t *testing.T) {
	del, err := resolveRmFlag(true, false)
	if err != nil {
		t.Fatalf("resolveRmFlag(true, false) = %v, want nil error", err)
	}
	if !del {
		t.Error("autoDelete = false, want true — --rm triggers delete-on-exit")
	}
}

func TestResolveRmFlagNoRmPersists(t *testing.T) {
	del, err := resolveRmFlag(false, false)
	if err != nil || del {
		t.Errorf("resolveRmFlag(false, false) = (%v, %v), want (false, nil)", del, err)
	}
}

func TestNewRunCmdFlagsAlignToContract(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newRunCmd(store)
	for _, f := range []struct{ long, short string }{
		{"cpus", "c"}, {"memory", "m"}, {"volume", "v"}, {"publish", "p"},
		{"env", "e"}, {"detach", "d"}, {"interactive", "i"}, {"tty", "t"},
		{"workdir", "w"}, {"user", "u"},
	} {
		fl := cmd.Flags().Lookup(f.long)
		if fl == nil {
			t.Errorf("--%s not registered", f.long)
			continue
		}
		if fl.Shorthand != f.short {
			t.Errorf("--%s shorthand = %q, want %q", f.long, fl.Shorthand, f.short)
		}
	}
}

// === runRun: --rm / -d interaction ===

func TestRunRunRejectsRmWithDetach(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	err := runRun(store, "base", nil, "", true, 0, 0, "open", nil, nil, false, "", nil, "", true)
	if err == nil {
		t.Fatal("runRun() = nil, want an error for --rm with --detach")
	}
	if !strings.Contains(err.Error(), "--rm requires a foreground command") {
		t.Errorf("error = %q, want mention of the foreground requirement", err)
	}
}

