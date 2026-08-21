package cli

import (
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

// TestPolicyReportsWhereNetworkIsEnforced is the field that matters most on
// this screen. On Firecracker the filter is a host-side chain and is therefore
// a boundary; on applevz it is iptables inside the guest, which a root process
// in the sandbox can remove. Reporting them identically would imply a guarantee
// that does not exist.
func TestPolicyReportsWhereNetworkIsEnforced(t *testing.T) {
	fc := PolicyFor(&state.VM{Backend: "firecracker", NetPolicy: "deny"})
	if fc.Network.EnforcedAt != "host" {
		t.Errorf("firecracker enforced_at = %q, want host", fc.Network.EnforcedAt)
	}
	vz := PolicyFor(&state.VM{Backend: "applevz", NetPolicy: "deny"})
	if vz.Network.EnforcedAt != "guest" {
		t.Errorf("applevz enforced_at = %q, want guest", vz.Network.EnforcedAt)
	}
}

func TestPolicyParsesAllowDomains(t *testing.T) {
	p := PolicyFor(&state.VM{Backend: "firecracker", NetPolicy: "allow:github.com,npmjs.org"})
	if p.Network.Mode != "allow" {
		t.Errorf("mode = %q, want allow", p.Network.Mode)
	}
	if len(p.Network.Domains) != 2 {
		t.Errorf("domains = %v, want two", p.Network.Domains)
	}
}

// An unparseable stored policy must not be reported as something reassuring.
func TestPolicyUnparseableFallsBackToOpen(t *testing.T) {
	p := PolicyFor(&state.VM{Backend: "firecracker", NetPolicy: "not-a-policy"})
	if p.Network.Mode == "deny" || p.Network.Mode == "allow" {
		t.Errorf("mode = %q; an unparseable policy must not claim to restrict anything", p.Network.Mode)
	}
}

// Root is always true — that is the product. Stating it explicitly stops a
// reader inferring an unprivileged sandbox from its absence.
func TestPolicyAlwaysReportsRoot(t *testing.T) {
	if !PolicyFor(&state.VM{Backend: "applevz"}).Privileges.Root {
		t.Error("the workload always runs as root; the policy view must say so")
	}
}

func TestPolicyPrefersSpecResources(t *testing.T) {
	p := PolicyFor(&state.VM{
		Backend: "firecracker", Cpus: 1, MemoryMB: 512,
		Spec: &state.VMSpec{Cpus: 4, MemoryMB: 2048, Seccomp: "strict"},
	})
	if p.Resources.Cpus != 4 || p.Resources.MemoryMB != 2048 {
		t.Errorf("resources = %+v, want the declared spec values", p.Resources)
	}
	if p.Privileges.Seccomp != "strict" {
		t.Errorf("seccomp = %q, want strict", p.Privileges.Seccomp)
	}
}

// Falling back to the live VM fields matters for VMs created before the spec
// was persisted.
func TestPolicyFallsBackToLiveFields(t *testing.T) {
	p := PolicyFor(&state.VM{Backend: "firecracker", Cpus: 2, MemoryMB: 1024})
	if p.Resources.Cpus != 2 || p.Resources.MemoryMB != 1024 {
		t.Errorf("resources = %+v, want the live VM values", p.Resources)
	}
}
