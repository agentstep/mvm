package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

func TestResolveRunNameWithExplicitName(t *testing.T) {
	name, ephemeral := resolveRunName("mybox", map[string]bool{})
	if name != "mybox" {
		t.Errorf("name = %q, want mybox", name)
	}
	if ephemeral {
		t.Error("ephemeral = true, want false for an explicit --name")
	}
}

func TestResolveRunNameGeneratesWhenEmpty(t *testing.T) {
	name, ephemeral := resolveRunName("", map[string]bool{})
	if name == "" {
		t.Error("name is empty, want a generated name")
	}
	if !ephemeral {
		t.Error("ephemeral = false, want true when no --name given")
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

func TestRunRunRejectsCustomImageOnAppleVZ(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")

	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "", false)
	if err == nil {
		t.Fatal("runRun() = nil, want an error for a non-base image on applevz")
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
	if err == nil {
		t.Fatal("resolveRmFlag(true, true) = nil error, want an error for --rm with --detach")
	}
	if !strings.Contains(err.Error(), "--rm requires a foreground command") {
		t.Errorf("error = %q, want mention of the foreground requirement", err)
	}
}

func TestResolveRmFlagForegroundWarns(t *testing.T) {
	warn, err := resolveRmFlag(true, false)
	if err != nil {
		t.Fatalf("resolveRmFlag(true, false) = %v, want nil error", err)
	}
	if !warn {
		t.Error("warn = false, want true for --rm in foreground mode")
	}
}

func TestResolveRmFlagNoRmNoWarning(t *testing.T) {
	if warn, err := resolveRmFlag(false, false); err != nil || warn {
		t.Errorf("resolveRmFlag(false, false) = (%v, %v), want (false, nil)", warn, err)
	}
	if warn, err := resolveRmFlag(false, true); err != nil || warn {
		t.Errorf("resolveRmFlag(false, true) = (%v, %v), want (false, nil)", warn, err)
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

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestRunRunRmForegroundWarnsThenContinues(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")
	// Force requireDaemon() to fail fast and deterministically, rather than
	// depending on whether this machine happens to have a real mvm daemon
	// running on the default socket (see Global Constraints).
	t.Setenv("MVM_REMOTE", "http://127.0.0.1:1")
	t.Setenv("MVM_API_KEY", "")

	stderr := captureStderr(t, func() {
		_ = runRun(store, "base", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "", true)
	})
	if !strings.Contains(stderr, "--rm has no effect in foreground mode") {
		t.Errorf("stderr = %q, want the --rm no-op warning", stderr)
	}
}
