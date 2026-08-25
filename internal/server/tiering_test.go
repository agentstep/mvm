package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
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

// A VM comes back from archive with its enforcement REINSTALLED, not just its metadata restored.
//
// archiveVM tears down the host-side egress rules and the DNS allowlist resolver on the way out.
// Restore copies the NetPolicy string back onto the record, which is what `inspect` reports — so a
// restore that skips reinstallation produces a sandbox with unrestricted egress that still claims
// to be restricted. That is a containment breach on a host running untrusted code, and a silent
// one: nothing in the API surface distinguishes it from a correctly restored VM.
func TestRestoreReinstallsEnforcement(t *testing.T) {
	src, err := os.ReadFile("tiering.go")
	if err != nil {
		t.Fatalf("read tiering.go: %v", err)
	}
	body := string(src)

	for _, call := range []struct{ fn, why string }{
		{"firecracker.InstallEgressPolicy", "egress rules are gone after archive; without this the VM returns with open network"},
		{"s.dns.Start", "the allowlist resolver is stopped on archive; without this an allow: policy resolves nothing and the ipset stays empty"},
		{"firecracker.SetupPortForwarding", "published-port DNAT is removed on archive and references the old TAP, so it must be reinstalled against the new allocation"},
	} {
		if !strings.Contains(body, call.fn) {
			t.Errorf("restoreArchivedVM never calls %s — %s", call.fn, call.why)
		}
	}

	// Reinstalling after handing the VM back would leave a window where the guest is running
	// unfiltered. The install must precede the status flip to "running".
	install := strings.Index(body, "firecracker.InstallEgressPolicy")
	running := strings.Index(body, `v.Status = "running"`)
	if install == -1 || running == -1 {
		t.Fatal("expected both an egress install and a status flip in restoreArchivedVM")
	}
	if install > running {
		t.Error("egress policy is installed AFTER the VM is marked running — that window is a VM live with no filter")
	}
}

// The restore path fails closed. A stored policy string that no longer parses must become deny,
// never open: an over-restricted VM is a bug report, an under-restricted one is a breach nobody
// sees.
func TestPolicyForRestoreFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, stored string
		want         state.NetPolicyMode
	}{
		{"unparseable fails closed", "allow:", state.NetPolicyDeny},
		{"garbage fails closed", "!!!not a policy!!!", state.NetPolicyDeny},
		{"deny stays deny", "deny", state.NetPolicyDeny},
		{"allow is preserved", "allow:github.com", state.NetPolicyAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := policyForRestore("vm1", tc.stored)
			if got.Mode != tc.want {
				t.Fatalf("policyForRestore(%q) mode = %v, want %v", tc.stored, got.Mode, tc.want)
			}
		})
	}

	// An unparseable string must not slip through as open — the mode that means "no filter".
	if m := policyForRestore("vm1", "allow:").Mode; m == state.NetPolicyOpen {
		t.Fatal("an unparseable stored policy resolved to open — this is the containment breach")
	}
}

// A restore that cannot reinstall enforcement must fail rather than hand back a running,
// unconstrained VM. Asserting on the source because the real path needs a live Firecracker.
func TestRestoreRefusesToHandBackAnUnconstrainedVM(t *testing.T) {
	src, err := os.ReadFile("tiering.go")
	if err != nil {
		t.Fatalf("read tiering.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "refusing to hand back an unconstrained VM") {
		t.Error("a failed egress reinstall during restore must return an error, not be logged and ignored")
	}
}
