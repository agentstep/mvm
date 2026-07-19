package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// === runRun: applevz custom-image guard ===

func TestRunRunRejectsCustomImageOnAppleVZ(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")

	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "")
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

	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "")
	if err == nil {
		t.Fatal("runRun() = nil, want the corrupt-state load error surfaced (not silently defaulting to firecracker and booting the wrong rootfs)")
	}
}
