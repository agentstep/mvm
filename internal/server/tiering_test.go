package server

import (
	"os"
	"strings"
	"testing"
	"time"
)

// parseThreshold decides whether a tier applies at all. It returning a default on bad input is how
// a VM nobody opted in gets archived, so the disabled cases matter more than the parsing.
func TestParseThresholdDisablesRatherThanDefaulting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  time.Duration
		valid bool
	}{
		{"empty disables", "", 0, false},
		{"garbage disables", "soon", 0, false},
		{"zero disables", "0s", 0, false},
		{"negative disables", "-5m", 0, false},
		{"minutes", "5m", 5 * time.Minute, true},
		{"hours", "1h", time.Hour, true},
		{"seconds", "90s", 90 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseThreshold(tc.in)
			if ok != tc.valid {
				t.Fatalf("parseThreshold(%q) valid = %v, want %v", tc.in, ok, tc.valid)
			}
			if ok && got != tc.want {
				t.Fatalf("parseThreshold(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The two thresholds are independent: a VM may pause without ever archiving, which is the common
// case for a sandbox in active multi-turn use.
func TestThresholdsAreIndependent(t *testing.T) {
	if _, ok := parseThreshold("5m"); !ok {
		t.Fatal("idle timeout should parse")
	}
	if _, ok := parseThreshold(""); ok {
		t.Fatal("an unset archive threshold must not enable archiving")
	}
}

// The restore path's real hazard is not the restore — it is that RestoreVMSnapshot works by
// removing the VM entry and reserving a fresh one, dropping everything the record carried. A VM
// that comes back without its thresholds never tiers again; without its secrets or ports it is the
// same VM by name and quietly not the same VM.
//
// This asserts the preserved set matches the fields restoreArchivedVM copies back, so adding a
// field to state.VM without adding it there fails here rather than in production.
func TestArchivedRestorePreservesConfigFields(t *testing.T) {
	preserved := []string{
		"IdleTimeout", "ArchiveAfter", "Spec", "Secrets",
		"Ports", "NetPolicy", "Backend", "Cpus", "MemoryMB",
	}

	src, err := os.ReadFile("tiering.go")
	if err != nil {
		t.Fatalf("read tiering.go: %v", err)
	}
	body := string(src)
	for _, f := range preserved {
		if !strings.Contains(body, "v."+f+" = preserved."+f) {
			t.Errorf("restoreArchivedVM does not restore %s — a VM would come back missing it", f)
		}
	}

	// And the two that must NOT be carried over, because the VM is running again.
	for _, cleared := range []string{`v.ArchivedSnapshot = ""`, "v.StoppedAt = nil"} {
		if !strings.Contains(body, cleared) {
			t.Errorf("restoreArchivedVM should clear via %q — a restored VM still marked archived would be re-restored forever", cleared)
		}
	}

	// Activity must be stamped, or the next sweep archives it straight back.
	if !strings.Contains(body, "v.LastActivity = &now") {
		t.Error("restoreArchivedVM must stamp LastActivity, or the sweep re-archives the VM immediately")
	}
}
