package firecracker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestEgressNamesAreKeyedOnIndex(t *testing.T) {
	if got := EgressChainName(0); got != "MVM-EGRESS-0" {
		t.Errorf("EgressChainName(0) = %q, want MVM-EGRESS-0", got)
	}
	if got := EgressChainName(7); got != "MVM-EGRESS-7" {
		t.Errorf("EgressChainName(7) = %q, want MVM-EGRESS-7", got)
	}
	if got := EgressIPSetName(3); got != "mvm-allow-3" {
		t.Errorf("EgressIPSetName(3) = %q, want mvm-allow-3", got)
	}
	if got := EgressIPSetName6(3); got != "mvm-allow6-3" {
		t.Errorf("EgressIPSetName6(3) = %q, want mvm-allow6-3", got)
	}
}

// TestEgressDenyAcceptsNothingTheGuestOriginates pins what "deny" means.
//
// This replaces an earlier guard that asserted the deny ruleset contained no ACCEPT string at all.
// That was a blunt proxy for the real property: the original defect it guarded was a DNS carve-out
// (--dport 53) leaving a working exfiltration channel under a policy named "deny". The string check
// also forbade the one accept that is not a hole.
//
// The contract now: nothing the GUEST originates is accepted, and replies on connections opened
// from outside are. A guest-originated flow is dropped at its SYN, so conntrack never promotes it
// past NEW — an ESTABLISHED,RELATED accept can therefore only match a flow whose first packet came
// from outside, i.e. a port the operator deliberately published. Without it, publishing a port on a
// deny VM was silently dead: the inbound SYN arrived via DNAT but the guest's SYN-ACK left on tapN
// and was dropped.
//
// This does widen deny: a published port on a deny sandbox is now a working channel in and out, and
// that is a wider one than the DNS hole this test originally closed. It is allowed deliberately —
// publishing is an explicit act — and belongs in the docs beside the ingress axis, not forbidden
// here.
func TestEgressDenyAcceptsNothingTheGuestOriginates(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := EgressInstallScript(alloc, state.ParsedNetPolicy{Mode: state.NetPolicyDeny})

	if !strings.Contains(script, `-A "$CHAIN" -j DROP`) {
		t.Error("deny ruleset must still end in an unconditional DROP")
	}

	// The DNS hole this test was written for must stay closed.
	if strings.Contains(script, "53") {
		t.Errorf("deny ruleset must not carve out DNS, got:\n%s", script)
	}

	// Exactly one accept, and only the conntrack form. Any other ACCEPT — a port, a protocol, an
	// address — would be a hole for guest-originated traffic.
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "ACCEPT") {
			continue
		}
		if !strings.Contains(line, "--ctstate ESTABLISHED,RELATED") {
			t.Errorf("deny ruleset has an accept that is not the conntrack reply rule — that is a hole for guest-originated traffic:\n%s", line)
		}
	}

	// Ordering is load-bearing: iptables takes the first match, so an accept placed after the
	// unconditional DROP would never run and published ports would stay dead.
	accept := strings.Index(script, "--ctstate ESTABLISHED,RELATED")
	drop := strings.Index(script, `-A "$CHAIN" -j DROP`)
	if accept == -1 {
		t.Fatal("deny ruleset accepts no established replies — a published port's SYN-ACK is dropped and the port is silently dead")
	}
	if accept > drop {
		t.Error("the established-reply accept comes after the DROP, so it never matches")
	}
}

func TestEgressInstallJumpsFromBothForwardAndInput(t *testing.T) {
	alloc := state.AllocateNet(2)
	script := EgressInstallScript(alloc, state.ParsedNetPolicy{Mode: state.NetPolicyDeny})

	if !strings.Contains(script, `-I FORWARD 1 -i "$TAP" -j "$CHAIN"`) {
		t.Error("install must jump from FORWARD (guest -> internet)")
	}
	if !strings.Contains(script, `-I INPUT 1 -i "$TAP" -j "$CHAIN"`) {
		t.Error("install must jump from INPUT (guest -> the Lima host itself)")
	}
	if !strings.Contains(script, "TAP=tap2") {
		t.Errorf("install must bind to the VM's own TAP device, got:\n%s", script)
	}
}

// TestEgressCoversIPv6 guards the gap the browser spike surfaced: enforcement
// drove iptables only, so ip6tables was wide open. That was survivable only
// because the guest has no IPv6 route — a filter that holds because the
// transport is unconfigured is not a filter. Guest DNS already returns AAAA
// records, so this is one routing change away from live.
func TestEgressCoversIPv6(t *testing.T) {
	for _, mode := range []state.NetPolicyMode{state.NetPolicyDeny, state.NetPolicyAllow} {
		policy := state.ParsedNetPolicy{Mode: mode}
		if mode == state.NetPolicyAllow {
			policy.Domains = []string{"github.com"}
		}
		script := EgressInstallScript(state.AllocateNet(0), policy)
		for _, ipt := range []string{"IPT=iptables", "IPT=ip6tables"} {
			if !strings.Contains(script, ipt) {
				t.Errorf("mode %v: script never sets %s, so that family is unfiltered:\n%s", mode, ipt, script)
			}
		}
	}
}

// TestEgressAllowUsesFamilySpecificIPSets pins that the v6 chain matches a v6
// set. An ipset's family is fixed at creation and `hash:ip` defaults to inet,
// which rejects an IPv6 address rather than storing it — so a single shared set
// would leave every AAAA the proxy resolved unreachable, silently.
func TestEgressAllowUsesFamilySpecificIPSets(t *testing.T) {
	script := EgressInstallScript(state.AllocateNet(1), state.ParsedNetPolicy{
		Mode: state.NetPolicyAllow, Domains: []string{"github.com"},
	})
	if !strings.Contains(script, "ipset create mvm-allow-1 hash:ip family inet timeout 600 -exist") {
		t.Errorf("missing the inet set:\n%s", script)
	}
	if !strings.Contains(script, "ipset create mvm-allow6-1 hash:ip family inet6 timeout 600 -exist") {
		t.Errorf("missing the inet6 set:\n%s", script)
	}
	if !strings.Contains(script, `--match-set mvm-allow6-1 dst -j ACCEPT`) {
		t.Errorf("v6 chain must match the v6 set:\n%s", script)
	}
}

// The DNS hole is for the VM's own gateway, which is IPv4-only, so the v6 chain
// must not carry a port-53 ACCEPT at all.
func TestEgressAllowV6ChainHasNoDNSHole(t *testing.T) {
	script := EgressInstallScript(state.AllocateNet(1), state.ParsedNetPolicy{
		Mode: state.NetPolicyAllow, Domains: []string{"github.com"},
	})
	_, v6Part, ok := strings.Cut(script, "IPT=ip6tables")
	if !ok {
		t.Fatal("no ip6tables section")
	}
	if strings.Contains(v6Part, "--dport 53") {
		t.Errorf("v6 chain must have no DNS carve-out (the resolver is IPv4-only):\n%s", v6Part)
	}
}

func TestEgressInstallIsIdempotent(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := EgressInstallScript(alloc, state.ParsedNetPolicy{Mode: state.NetPolicyDeny})

	flush := strings.Index(script, `-F "$CHAIN"`)
	appendRule := strings.Index(script, `-A "$CHAIN"`)
	if flush < 0 || appendRule < 0 || flush > appendRule {
		t.Errorf("install must flush the chain before appending rules, got:\n%s", script)
	}
	if !strings.Contains(script, `while sudo "$IPT" -C FORWARD -i "$TAP" -j "$CHAIN"`) {
		t.Error("install must drain duplicate FORWARD jumps left by an earlier run")
	}
}

func TestEgressAllowOnlyPermitsDNSToItsOwnGateway(t *testing.T) {
	alloc := state.AllocateNet(1) // TAPIP 172.16.0.5
	policy := state.ParsedNetPolicy{Mode: state.NetPolicyAllow, Domains: []string{"github.com"}}
	script := EgressInstallScript(alloc, policy)

	if !strings.Contains(script, "-p udp -d 172.16.0.5 --dport 53 -j ACCEPT") {
		t.Errorf("allow must permit DNS to the VM's own gateway, got:\n%s", script)
	}
	// Every DNS rule must be pinned to the gateway. An unpinned --dport 53
	// ACCEPT would let the guest query a public resolver directly and reach any
	// address it learned that way, bypassing the ipset.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "--dport 53") && !strings.Contains(line, "-d 172.16.0.5") {
			t.Errorf("unpinned DNS rule permits arbitrary resolvers: %q", line)
		}
	}
}

// TestEgressAllowNeverInterpolatesDomains enforces that domain strings reach
// the DNS proxy as Go values and never enter a shell command.
func TestEgressAllowNeverInterpolatesDomains(t *testing.T) {
	alloc := state.AllocateNet(0)
	policy := state.ParsedNetPolicy{Mode: state.NetPolicyAllow, Domains: []string{"github.com", "npmjs.org"}}
	script := EgressInstallScript(alloc, policy)

	for _, d := range policy.Domains {
		if strings.Contains(script, d) {
			t.Errorf("domain %q leaked into the iptables script:\n%s", d, script)
		}
	}
}

func TestEgressOpenReturnsRemoval(t *testing.T) {
	alloc := state.AllocateNet(0)
	open := EgressInstallScript(alloc, state.ParsedNetPolicy{Mode: state.NetPolicyOpen})
	if open != EgressRemoveScript(alloc) {
		t.Error("open policy must produce exactly the removal script")
	}
}

func TestEgressRemoveTearsDownEverything(t *testing.T) {
	script := EgressRemoveScript(state.AllocateNet(4))
	for _, want := range []string{
		`-D FORWARD -i "$TAP" -j "$CHAIN"`,
		`-D INPUT -i "$TAP" -j "$CHAIN"`,
		`-F "$CHAIN"`,
		`-X "$CHAIN"`,
		"ipset destroy mvm-allow-4",
		"ipset destroy mvm-allow6-4",
		"for IPT in iptables ip6tables",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("removal script missing %q, got:\n%s", want, script)
		}
	}
	// Cleanup is best-effort; `set -e` would abort the teardown partway through
	// and leak a chain that the next VM at this index inherits.
	if strings.Contains(script, "set -e") {
		t.Error("removal script must not use `set -e` — every step is best-effort")
	}
}

// failingExecutor records commands and always fails, for the paths where a
// policy that cannot be installed must abort rather than be swallowed. The
// success case reuses the package's existing recordingExecutor.
type failingExecutor struct{ err error }

func (e *failingExecutor) Run(string) (string, error) { return "", e.err }
func (e *failingExecutor) RunWithTimeout(c string, _ time.Duration) (string, error) {
	return e.Run(c)
}

func TestInstallEgressPolicyRunsOnTheHost(t *testing.T) {
	ex := &recordingExecutor{}
	alloc := state.AllocateNet(0)
	if err := InstallEgressPolicy(ex, alloc, state.ParsedNetPolicy{Mode: state.NetPolicyDeny}); err != nil {
		t.Fatalf("InstallEgressPolicy: %v", err)
	}
	if len(ex.commands) != 1 {
		t.Fatalf("expected 1 host command, got %d", len(ex.commands))
	}
	if !strings.Contains(ex.commands[0], "MVM-EGRESS-0") {
		t.Errorf("command did not install the VM's chain: %q", ex.commands[0])
	}
}

// TestInstallEgressPolicyPropagatesFailure pins that a policy which fails to
// install is a hard error. The previous implementation logged and continued,
// leaving the VM running with no filter at all.
func TestInstallEgressPolicyPropagatesFailure(t *testing.T) {
	ex := &failingExecutor{err: errors.New("iptables: permission denied")}
	if err := InstallEgressPolicy(ex, state.AllocateNet(0), state.ParsedNetPolicy{Mode: state.NetPolicyDeny}); err == nil {
		t.Fatal("a failed deny install must return an error, never be swallowed")
	}
}

func TestInstallEgressPolicyOpenToleratesFailure(t *testing.T) {
	ex := &failingExecutor{err: errors.New("no chain by that name")}
	if err := InstallEgressPolicy(ex, state.AllocateNet(0), state.ParsedNetPolicy{Mode: state.NetPolicyOpen}); err != nil {
		t.Fatalf("open policy must tolerate teardown failure, got: %v", err)
	}
}

func TestRemoveEgressPolicyUsesTheVMsIndex(t *testing.T) {
	ex := &recordingExecutor{}
	RemoveEgressPolicy(ex, &state.VM{Name: "web", NetIndex: 5})
	if len(ex.commands) != 1 {
		t.Fatalf("expected 1 host command, got %d", len(ex.commands))
	}
	if !strings.Contains(ex.commands[0], "MVM-EGRESS-5") || !strings.Contains(ex.commands[0], "mvm-allow-5") {
		t.Errorf("teardown did not target index 5: %q", ex.commands[0])
	}
}
