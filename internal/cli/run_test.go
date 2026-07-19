package cli

import (
	"fmt"
	"testing"
	"time"
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
