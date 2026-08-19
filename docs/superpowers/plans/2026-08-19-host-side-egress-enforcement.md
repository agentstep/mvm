# Host-Side Egress Enforcement (Firecracker) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move `--net-policy` enforcement for the Firecracker backend out of the guest and into the Lima host, so a root process inside the sandbox can no longer flush its own cage.

**Architecture:** Today `ApplyNetworkPolicyViaAgent` (`internal/firecracker/process.go:166`) tells the *guest* to run `iptables`. The guest runs as root, so `iptables -F` inside the sandbox removes the policy entirely. We replace this with a per-VM iptables chain in the Lima VM, jumped to from `FORWARD` and `INPUT` on `-i tapN`, installed **before the microVM boots**. Domain allowlists stop being one-shot `getent` lookups pinned at boot: a DNS proxy in the daemon answers only allowlisted names and feeds every answer's IPs into a per-VM ipset that the filter matches against.

**Tech Stack:** Go 1.26.1, iptables + ipset (inside the Lima VM), `github.com/miekg/dns` (new dependency, Task 4), existing `firecracker.Executor` abstraction.

## Global Constraints

- **Enforcement never runs inside the guest.** No task may add an `agentExec`/`agent.Exec` call that issues `iptables`. The guest is the untrusted party.
- **A policy that fails to install is a hard error.** Unlike the current code, which does `log.Printf` and continues, a failed `deny`/`allow:` install must fail VM creation. A silently-unapplied policy is the bug being fixed.
- **Chains and ipsets are keyed on `NetAllocation.Index`, never on the VM name.** TAP device names (`tap0`, `tap1`, …) are derived from the index and are *reused* by the next VM at that index. A chain keyed on anything else leaks rules across VMs.
- **No user-controlled string is ever interpolated into an iptables command.** Domains reach the DNS proxy as Go values only. Port fields already go through `state.ValidatePort`.
- **Scope is the Firecracker backend only.** The Apple VZ path (`internal/cli/start.go:787`) has no host-side TAP and needs a different mechanism; it gets its own plan. Task 5 makes the VZ gap explicit rather than silently leaving it.
- **Every policy must cover IPv6 as well as IPv4.** The current implementation
  drives `iptables` only, so `ip6tables` is wide open. This is latent rather
  than exploitable today — the guest gets no IPv6 route, so v6 egress fails on
  its own — but it is one `ip=dhcp6` or one upstream image change away from
  silently bypassing `deny` and `allow:` completely. A filter that holds only
  because the transport happens to be unconfigured is not a filter. Verified
  during the 2026-08-19 browser spike: guest DNS returns AAAA records today,
  so `curl` without `-4` already prefers a v6 path that no policy governs.
- Existing test command: `make test` (`go test ./internal/... -v -race`).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/state/netpolicy.go` (create) | Parse + validate `open` / `deny` / `allow:<domains>` into a typed value. Domain syntax validation. |
| `internal/state/netpolicy_test.go` (create) | Parser and domain-validation tests. |
| `internal/firecracker/egress.go` (create) | Generate the host-side install/remove shell scripts. Pure string generation, no I/O. |
| `internal/firecracker/egress_test.go` (create) | Rule-generation tests, including the "deny means deny" regression guards. |
| `internal/firecracker/process.go` (modify) | Delete `ApplyNetworkPolicyViaAgent`. |
| `internal/server/routes.go` (modify) | Install pre-boot in `handleCreateVM`; drop the in-guest call from `postBootSetup`; remove on stop/delete. |
| `internal/egressdns/resolver.go` (create) | Per-VM DNS proxy: allowlist matching, upstream forwarding, ipset population. |
| `internal/egressdns/resolver_test.go` (create) | Allowlist matcher tests (label-boundary matching is the security-critical part). |
| `internal/cli/init.go` (modify) | Install `ipset` in the Lima VM. |
| `internal/cli/doctor.go` (modify) | Check `ipset`/`xt_set` availability and report the VZ gap. |
| `docs/networking.md` (create) | Document the trust model and the `deny` behavior change. |

---

### Task 1: Typed network policy parsing

Everything downstream needs a validated policy, and domain strings need to be provably safe before they go anywhere near a shell or an ipset command.

**Files:**
- Create: `internal/state/netpolicy.go`
- Test: `internal/state/netpolicy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `state.NetPolicyMode` (`NetPolicyOpen`, `NetPolicyDeny`, `NetPolicyAllow`), `state.ParsedNetPolicy{Mode NetPolicyMode; Domains []string}`, `state.ParseNetPolicy(s string) (ParsedNetPolicy, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/state/netpolicy_test.go`:

```go
package state

import "testing"

func TestParseNetPolicyModes(t *testing.T) {
	tests := []struct {
		in       string
		wantMode NetPolicyMode
		wantDoms []string
	}{
		{"", NetPolicyOpen, nil},
		{"open", NetPolicyOpen, nil},
		{"deny", NetPolicyDeny, nil},
		{"allow:github.com", NetPolicyAllow, []string{"github.com"}},
		{"allow:github.com,registry.npmjs.org", NetPolicyAllow, []string{"github.com", "registry.npmjs.org"}},
		{"allow: github.com , NPMJS.org ", NetPolicyAllow, []string{"github.com", "npmjs.org"}},
	}
	for _, tt := range tests {
		got, err := ParseNetPolicy(tt.in)
		if err != nil {
			t.Errorf("ParseNetPolicy(%q) returned error: %v", tt.in, err)
			continue
		}
		if got.Mode != tt.wantMode {
			t.Errorf("ParseNetPolicy(%q).Mode = %v, want %v", tt.in, got.Mode, tt.wantMode)
		}
		if len(got.Domains) != len(tt.wantDoms) {
			t.Errorf("ParseNetPolicy(%q).Domains = %v, want %v", tt.in, got.Domains, tt.wantDoms)
			continue
		}
		for i := range tt.wantDoms {
			if got.Domains[i] != tt.wantDoms[i] {
				t.Errorf("ParseNetPolicy(%q).Domains[%d] = %q, want %q", tt.in, i, got.Domains[i], tt.wantDoms[i])
			}
		}
	}
}

// TestParseNetPolicyRejectsShellMetacharacters is the load-bearing test.
// Domains flow into ipset/DNS handling; anything that could terminate a shell
// word must be refused at the parser, not defended against downstream.
func TestParseNetPolicyRejectsShellMetacharacters(t *testing.T) {
	bad := []string{
		"allow:$(rm -rf /)",
		"allow:`whoami`",
		"allow:github.com; rm -rf /",
		"allow:github.com && evil.com",
		"allow:github.com|evil.com",
		"allow:evil.com\nrm -rf /",
		"allow:-leadinghyphen.com",
		"allow:trailinghyphen-.com",
		"allow:double..dot.com",
		"allow:.leadingdot.com",
		"allow:has_underscore.com",
		"allow:has space.com",
	}
	for _, in := range bad {
		if _, err := ParseNetPolicy(in); err == nil {
			t.Errorf("ParseNetPolicy(%q) should have been rejected", in)
		}
	}
}

func TestParseNetPolicyRejectsEmptyAllowList(t *testing.T) {
	for _, in := range []string{"allow:", "allow:,", "allow: , "} {
		if _, err := ParseNetPolicy(in); err == nil {
			t.Errorf("ParseNetPolicy(%q) should have been rejected (no domains)", in)
		}
	}
}

func TestParseNetPolicyRejectsUnknownMode(t *testing.T) {
	for _, in := range []string{"denyall", "allow", "block", "ALLOW:github.com"} {
		if _, err := ParseNetPolicy(in); err == nil {
			t.Errorf("ParseNetPolicy(%q) should have been rejected", in)
		}
	}
}

func TestParseNetPolicyRejectsOverlongNames(t *testing.T) {
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := ParseNetPolicy("allow:" + string(long) + ".com"); err == nil {
		t.Error("a 64-character label should have been rejected (max 63)")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/state/ -run TestParseNetPolicy -v`
Expected: FAIL — `undefined: NetPolicyMode`, `undefined: ParseNetPolicy`.

- [ ] **Step 3: Write the implementation**

Create `internal/state/netpolicy.go`:

```go
package state

import (
	"fmt"
	"strings"
)

// NetPolicyMode is the enforcement mode for a VM's outbound traffic.
type NetPolicyMode int

const (
	// NetPolicyOpen applies no filter at all.
	NetPolicyOpen NetPolicyMode = iota
	// NetPolicyDeny drops every packet the guest originates. Unlike the old
	// in-guest implementation this includes DNS: a resolver the guest can
	// still reach is an exfiltration channel, so "deny" means deny.
	NetPolicyDeny
	// NetPolicyAllow drops everything except traffic to addresses that the
	// egress DNS proxy resolved for an allowlisted domain.
	NetPolicyAllow
)

// ParsedNetPolicy is a validated network policy. Construct it only via
// ParseNetPolicy — downstream code interpolates the result into privileged
// commands and relies on the validation having happened.
type ParsedNetPolicy struct {
	Mode    NetPolicyMode
	Domains []string // lowercased, syntactically valid; non-empty iff Mode == NetPolicyAllow
}

// ParseNetPolicy validates the wire format used by --net-policy and the
// daemon's CreateVMRequest.NetPolicy field: "", "open", "deny", or
// "allow:<comma-separated domains>".
func ParseNetPolicy(s string) (ParsedNetPolicy, error) {
	switch {
	case s == "" || s == "open":
		return ParsedNetPolicy{Mode: NetPolicyOpen}, nil
	case s == "deny":
		return ParsedNetPolicy{Mode: NetPolicyDeny}, nil
	case strings.HasPrefix(s, "allow:"):
		var domains []string
		for _, d := range strings.Split(strings.TrimPrefix(s, "allow:"), ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			if !ValidDomain(d) {
				return ParsedNetPolicy{}, fmt.Errorf("invalid domain %q in network policy", d)
			}
			domains = append(domains, strings.ToLower(d))
		}
		if len(domains) == 0 {
			return ParsedNetPolicy{}, fmt.Errorf("network policy %q lists no domains", s)
		}
		return ParsedNetPolicy{Mode: NetPolicyAllow, Domains: domains}, nil
	default:
		return ParsedNetPolicy{}, fmt.Errorf("unknown network policy %q (want open, deny, or allow:<domains>)", s)
	}
}

// ValidDomain reports whether s is a DNS name safe to hand to privileged
// tooling. Deliberately stricter than RFC 1035: ASCII letters, digits,
// hyphen and dot only, so no shell metacharacter, whitespace or newline can
// survive parsing. Underscores are refused too — they are legal in some
// records but never in a name a user would allowlist for egress.
func ValidDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9':
			case c == '-':
			default:
				return false
			}
		}
	}
	return true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/state/ -run TestParseNetPolicy -v`
Expected: PASS — all five test functions.

- [ ] **Step 5: Commit**

```bash
git add internal/state/netpolicy.go internal/state/netpolicy_test.go
git commit -m "feat(state): typed, validated network policy parsing"
```

---

### Task 2: Host-side rule generation

Pure script generation, fully unit-testable without Lima. This is where the security properties are decided.

**Files:**
- Create: `internal/firecracker/egress.go`
- Test: `internal/firecracker/egress_test.go`

**Interfaces:**
- Consumes: `state.ParsedNetPolicy`, `state.NetAllocation` (fields `Index`, `TAPDev`, `TAPIP`) from Task 1 and existing code.
- Produces: `firecracker.EgressChainName(index int) string`, `firecracker.EgressIPSetName(index int) string`, `firecracker.EgressIPSetName6(index int) string`, `firecracker.EgressInstallScript(alloc state.NetAllocation, policy state.ParsedNetPolicy) string`, `firecracker.EgressRemoveScript(alloc state.NetAllocation) string`.

Design notes the implementer needs:

- The chain is jumped to from **both** `FORWARD -i tapN` (guest → internet) and `INPUT -i tapN` (guest → the Lima host itself, e.g. the daemon's own listeners). Filtering only `FORWARD` would leave the Lima host reachable.
- Return traffic is `-o tapN` and is deliberately **not** matched, so published inbound ports (`-p 3000:3000`, DNAT'd in `SetupPortForwarding`) keep working under `deny`.
- vsock is not IP and never traverses these chains, so `mvm exec` stays available under `deny`. That is the intended behavior: the operator keeps control, the guest loses the network.
- The script is idempotent — it flushes the chain and de-duplicates the jumps on every run — so re-applying after a snapshot restore converges instead of stacking rules.
- `NetPolicyOpen` returns the *removal* script, so callers have a single entry point.
- **The same chain is installed twice: once via `iptables`, once via `ip6tables`.** Both take identical rules, so the generator emits one rule body and loops over the two binaries. `ipset` needs two sets, though — `hash:ip` is `family inet` by default and *rejects* an IPv6 address, so the v6 set must be created with `family inet6` and matched from the `ip6tables` chain only.

Below, `$IPT` is the loop variable holding `iptables` or `ip6tables`.

- [ ] **Step 1: Write the failing test**

Create `internal/firecracker/egress_test.go`:

```go
package firecracker

import (
	"strings"
	"testing"

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

// TestEgressDenyHasNoAcceptAtAll is a regression guard on the behavior change.
// The old in-guest ruleset punched holes for DNS (--dport 53), which left a
// working exfiltration channel under a policy literally named "deny".
func TestEgressDenyHasNoAcceptAtAll(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := EgressInstallScript(alloc, state.ParsedNetPolicy{Mode: state.NetPolicyDeny})

	if !strings.Contains(script, `-A "$CHAIN" -j DROP`) {
		t.Error("deny ruleset must end in an unconditional DROP")
	}
	if strings.Contains(script, "ACCEPT") {
		t.Errorf("deny ruleset must contain no ACCEPT rule, got:\n%s", script)
	}
	if strings.Contains(script, "53") {
		t.Errorf("deny ruleset must not carve out DNS, got:\n%s", script)
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

// TestEgressCoversIPv6 is the regression guard on the gap the 2026-08-19
// browser spike surfaced: enforcement drove iptables only, so ip6tables was
// wide open. That was survivable only because the guest has no IPv6 route —
// a filter that holds because the transport is unconfigured is not a filter.
// Guest DNS already returns AAAA records, so this is one routing change away
// from live.
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

// TestEgressAllowUsesFamilySpecificIPSets pins that the v6 chain matches a
// v6 set. An ipset's family is fixed at creation and `hash:ip` defaults to
// inet, which rejects an IPv6 address rather than storing it — so a single
// shared set would silently never match any AAAA the proxy resolved.
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

// The DNS hole is for the VM's own gateway, which is IPv4-only, so the v6
// chain must not carry a port-53 ACCEPT at all.
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

func TestEgressAllowUsesIPSetAndDefaultsToDrop(t *testing.T) {
	alloc := state.AllocateNet(1)
	policy := state.ParsedNetPolicy{Mode: state.NetPolicyAllow, Domains: []string{"github.com"}}
	script := EgressInstallScript(alloc, policy)

	if !strings.Contains(script, "ipset create mvm-allow-1 hash:ip family inet timeout 600 -exist") {
		t.Errorf("allow must create the VM's ipset before referencing it, got:\n%s", script)
	}
	if !strings.Contains(script, `-m set --match-set mvm-allow-1 dst -j ACCEPT`) {
		t.Error("allow must accept destinations present in the ipset")
	}
	if !strings.HasSuffix(strings.TrimSpace(lastRule(script)), "-j DROP") {
		t.Errorf("allow must end in a default DROP, got last rule: %q", lastRule(script))
	}
}

// TestEgressAllowOnlyPermitsDNSToItsOwnGateway proves the guest cannot bypass
// the proxy by querying a public resolver directly.
func TestEgressAllowOnlyPermitsDNSToItsOwnGateway(t *testing.T) {
	alloc := state.AllocateNet(1) // TAPIP 172.16.0.5
	policy := state.ParsedNetPolicy{Mode: state.NetPolicyAllow, Domains: []string{"github.com"}}
	script := EgressInstallScript(alloc, policy)

	if !strings.Contains(script, "-p udp -d 172.16.0.5 --dport 53 -j ACCEPT") {
		t.Errorf("allow must permit DNS to the VM's own gateway, got:\n%s", script)
	}
	// Every DNS rule must be pinned to the gateway. An unpinned --dport 53
	// ACCEPT would let the guest query a public resolver directly and reach
	// any address it learned that way, bypassing the ipset entirely.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "--dport 53") && !strings.Contains(line, "-d 172.16.0.5") {
			t.Errorf("unpinned DNS rule permits arbitrary resolvers: %q", line)
		}
	}
}

// TestEgressAllowNeverInterpolatesDomains enforces the constraint that domain
// strings reach the DNS proxy as Go values and never enter a shell command.
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
	} {
		if !strings.Contains(script, want) {
			t.Errorf("removal script missing %q, got:\n%s", want, script)
		}
	}
	// Cleanup is best-effort; `set -e` would abort the teardown partway
	// through and leak a chain that the next VM at this index inherits.
	if strings.Contains(script, "set -e") {
		t.Error("removal script must not use `set -e` — every step is best-effort")
	}
}

// lastRule returns the final `iptables -A` line in a script.
func lastRule(script string) string {
	lines := strings.Split(strings.TrimSpace(script), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], `-A "$CHAIN"`) {
			return lines[i]
		}
	}
	return ""
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/firecracker/ -run TestEgress -v`
Expected: FAIL — `undefined: EgressChainName`, `undefined: EgressInstallScript`, `undefined: EgressRemoveScript`, `undefined: EgressIPSetName`.

- [ ] **Step 3: Write the implementation**

Create `internal/firecracker/egress.go`:

```go
package firecracker

import (
	"fmt"
	"strings"

	"github.com/agentstep/mvm/internal/state"
)

// EgressChainName returns the iptables chain holding one VM's egress rules.
//
// Keyed on the network index rather than the VM name because the TAP device
// name is derived from the index and gets reused: after VM "a" at index 0 is
// deleted, VM "b" may be allocated index 0 and inherit tap0. A chain keyed on
// anything else would leave a stale rule matching the new VM's traffic.
func EgressChainName(index int) string {
	return fmt.Sprintf("MVM-EGRESS-%d", index)
}

// EgressIPSetName returns the IPv4 ipset holding the addresses the egress DNS
// proxy has resolved for this VM's allowlisted domains.
func EgressIPSetName(index int) string {
	return fmt.Sprintf("mvm-allow-%d", index)
}

// EgressIPSetName6 is the IPv6 counterpart. It has to be a separate set: an
// ipset is created with a fixed family, and `hash:ip` defaults to inet, which
// rejects an IPv6 address outright rather than storing it.
func EgressIPSetName6(index int) string {
	return fmt.Sprintf("mvm-allow6-%d", index)
}

// egressBinaries is every netfilter binary a policy must be installed into.
//
// Filtering only IPv4 would leave a policy that holds solely because the guest
// happens to have no IPv6 route — one image or boot-arg change away from
// silently allowing everything it was meant to block.
var egressBinaries = []string{"iptables", "ip6tables"}

// egressChainPreamble creates the chain, empties it, and re-points the jumps.
// Expects $IPT, $CHAIN and $TAP to be set. Draining the jumps in a loop (rather
// than a single -D) makes the script converge even if an earlier crashed run
// left duplicates behind.
const egressChainPreamble = `sudo "$IPT" -N "$CHAIN" 2>/dev/null || true
sudo "$IPT" -F "$CHAIN"
while sudo "$IPT" -C FORWARD -i "$TAP" -j "$CHAIN" 2>/dev/null; do
    sudo "$IPT" -D FORWARD -i "$TAP" -j "$CHAIN"
done
while sudo "$IPT" -C INPUT -i "$TAP" -j "$CHAIN" 2>/dev/null; do
    sudo "$IPT" -D INPUT -i "$TAP" -j "$CHAIN"
done
sudo "$IPT" -I FORWARD 1 -i "$TAP" -j "$CHAIN"
sudo "$IPT" -I INPUT 1 -i "$TAP" -j "$CHAIN"
`

// EgressInstallScript returns a shell script, to be run on the Firecracker
// host (the Lima VM), that enforces policy on traffic originating in the
// guest.
//
// Enforcement deliberately lives outside the guest. The guest runs as root —
// that is the product — so any rule installed inside it can be flushed by the
// process it is meant to contain.
//
// Only traffic arriving on the VM's TAP device is filtered (-i tapN). Replies
// to the outside world, and DNAT'd inbound connections to published ports,
// arrive on other interfaces and are untouched; vsock is not IP and never
// reaches these chains, so `mvm exec` keeps working under every policy.
func EgressInstallScript(alloc state.NetAllocation, policy state.ParsedNetPolicy) string {
	if policy.Mode == state.NetPolicyOpen {
		return EgressRemoveScript(alloc)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "set -e\nCHAIN=%s\nTAP=%s\n", EgressChainName(alloc.Index), alloc.TAPDev)

	if policy.Mode == state.NetPolicyAllow {
		// Created empty and populated by internal/egressdns as the proxy
		// answers queries. Empty behaves exactly like deny, so there is no
		// window in which the VM is unprotected. Two sets because an ipset's
		// family is fixed at creation and inet rejects IPv6 addresses.
		fmt.Fprintf(&b, "sudo ipset create %s hash:ip family inet timeout 600 -exist\n", EgressIPSetName(alloc.Index))
		fmt.Fprintf(&b, "sudo ipset flush %s\n", EgressIPSetName(alloc.Index))
		fmt.Fprintf(&b, "sudo ipset create %s hash:ip family inet6 timeout 600 -exist\n", EgressIPSetName6(alloc.Index))
		fmt.Fprintf(&b, "sudo ipset flush %s\n", EgressIPSetName6(alloc.Index))
	}

	// Identical rules go into both the v4 and v6 chains. Only the ipset
	// differs, since each set is family-locked.
	for _, ipt := range egressBinaries {
		set := EgressIPSetName(alloc.Index)
		if ipt == "ip6tables" {
			set = EgressIPSetName6(alloc.Index)
		}
		fmt.Fprintf(&b, "IPT=%s\n", ipt)
		b.WriteString(egressChainPreamble)

		switch policy.Mode {
		case state.NetPolicyDeny:
			// No carve-outs, not even DNS: a reachable resolver is an
			// exfiltration channel under a policy named "deny".
			b.WriteString("sudo \"$IPT\" -A \"$CHAIN\" -j DROP\n")

		case state.NetPolicyAllow:
			b.WriteString("sudo \"$IPT\" -A \"$CHAIN\" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n")
			// DNS is permitted only to this VM's own gateway, where the egress
			// proxy listens — never to a public resolver, which would let the
			// guest resolve blocked names and bypass the ipset. The gateway is
			// IPv4-only, so the v6 chain gets no DNS hole at all.
			if ipt == "iptables" {
				fmt.Fprintf(&b, "sudo \"$IPT\" -A \"$CHAIN\" -p udp -d %s --dport 53 -j ACCEPT\n", alloc.TAPIP)
				fmt.Fprintf(&b, "sudo \"$IPT\" -A \"$CHAIN\" -p tcp -d %s --dport 53 -j ACCEPT\n", alloc.TAPIP)
			}
			fmt.Fprintf(&b, "sudo \"$IPT\" -A \"$CHAIN\" -m set --match-set %s dst -j ACCEPT\n", set)
			b.WriteString("sudo \"$IPT\" -A \"$CHAIN\" -j DROP\n")
		}
	}

	b.WriteString("echo EGRESS_OK\n")
	return b.String()
}

// EgressRemoveScript returns a shell script that tears down a VM's egress
// filter. Every step is best-effort — deliberately no `set -e`, because an
// early failure must not leave a chain behind for the next VM to inherit at
// this index.
func EgressRemoveScript(alloc state.NetAllocation) string {
	return fmt.Sprintf(`CHAIN=%s
TAP=%s
for IPT in iptables ip6tables; do
    while sudo "$IPT" -C FORWARD -i "$TAP" -j "$CHAIN" 2>/dev/null; do
        sudo "$IPT" -D FORWARD -i "$TAP" -j "$CHAIN"
    done
    while sudo "$IPT" -C INPUT -i "$TAP" -j "$CHAIN" 2>/dev/null; do
        sudo "$IPT" -D INPUT -i "$TAP" -j "$CHAIN"
    done
    sudo "$IPT" -F "$CHAIN" 2>/dev/null || true
    sudo "$IPT" -X "$CHAIN" 2>/dev/null || true
done
sudo ipset destroy %s 2>/dev/null || true
sudo ipset destroy %s 2>/dev/null || true
echo EGRESS_REMOVED
`, EgressChainName(alloc.Index), alloc.TAPDev, EgressIPSetName(alloc.Index), EgressIPSetName6(alloc.Index))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/firecracker/ -run TestEgress -v`
Expected: PASS — nine test functions.

- [ ] **Step 5: Commit**

```bash
git add internal/firecracker/egress.go internal/firecracker/egress_test.go
git commit -m "feat(firecracker): host-side egress rule generation"
```

---

### Task 3: Wire enforcement into the VM lifecycle

Install pre-boot, tear down on stop and delete, and delete the in-guest path so it can't be called again.

**Files:**
- Modify: `internal/firecracker/process.go:165-188` (delete `ApplyNetworkPolicyViaAgent`)
- Modify: `internal/firecracker/egress.go` (add the two executor helpers)
- Modify: `internal/server/routes.go` (`postBootSetup` ~line 229, `handleCreateVM` ~line 310, stop handler ~line 679, restore handler ~line 830)
- Test: `internal/firecracker/egress_test.go` (extend)

**Interfaces:**
- Consumes: `EgressInstallScript`, `EgressRemoveScript` (Task 2); `state.ParseNetPolicy` (Task 1); the existing `Executor` interface (`Run(string) (string, error)`).
- Produces: `firecracker.InstallEgressPolicy(exec Executor, alloc state.NetAllocation, policy state.ParsedNetPolicy) error`, `firecracker.RemoveEgressPolicy(exec Executor, vm *state.VM)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/firecracker/egress_test.go`:

```go
// recordingExecutor captures the commands a helper would run, so lifecycle
// wiring can be tested without a Lima VM.
type recordingExecutor struct {
	cmds []string
	err  error
}

func (e *recordingExecutor) Run(command string) (string, error) {
	e.cmds = append(e.cmds, command)
	return "", e.err
}

func (e *recordingExecutor) RunWithTimeout(command string, _ time.Duration) (string, error) {
	return e.Run(command)
}

func TestInstallEgressPolicyRunsOnTheHost(t *testing.T) {
	ex := &recordingExecutor{}
	alloc := state.AllocateNet(0)
	err := InstallEgressPolicy(ex, alloc, state.ParsedNetPolicy{Mode: state.NetPolicyDeny})
	if err != nil {
		t.Fatalf("InstallEgressPolicy: %v", err)
	}
	if len(ex.cmds) != 1 {
		t.Fatalf("expected 1 host command, got %d: %v", len(ex.cmds), ex.cmds)
	}
	if !strings.Contains(ex.cmds[0], "MVM-EGRESS-0") {
		t.Errorf("command did not install the VM's chain: %q", ex.cmds[0])
	}
}

// TestInstallEgressPolicyPropagatesFailure pins the constraint that a policy
// which fails to install is a hard error. The previous implementation logged
// and continued, which left the VM running with no filter at all.
func TestInstallEgressPolicyPropagatesFailure(t *testing.T) {
	ex := &recordingExecutor{err: errors.New("iptables: permission denied")}
	err := InstallEgressPolicy(ex, state.AllocateNet(0), state.ParsedNetPolicy{Mode: state.NetPolicyDeny})
	if err == nil {
		t.Fatal("a failed deny install must return an error, never be swallowed")
	}
}

// An open policy has nothing to enforce, so a teardown failure on a host that
// never had the chain must not fail VM creation.
func TestInstallEgressPolicyOpenToleratesFailure(t *testing.T) {
	ex := &recordingExecutor{err: errors.New("no chain by that name")}
	if err := InstallEgressPolicy(ex, state.AllocateNet(0), state.ParsedNetPolicy{Mode: state.NetPolicyOpen}); err != nil {
		t.Fatalf("open policy must tolerate teardown failure, got: %v", err)
	}
}

func TestRemoveEgressPolicyUsesTheVMsIndex(t *testing.T) {
	ex := &recordingExecutor{}
	RemoveEgressPolicy(ex, &state.VM{Name: "web", NetIndex: 5})
	if len(ex.cmds) != 1 {
		t.Fatalf("expected 1 host command, got %d: %v", len(ex.cmds), ex.cmds)
	}
	if !strings.Contains(ex.cmds[0], "MVM-EGRESS-5") || !strings.Contains(ex.cmds[0], "mvm-allow-5") {
		t.Errorf("teardown did not target index 5: %q", ex.cmds[0])
	}
}
```

Add `"errors"` and `"time"` to that file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/firecracker/ -run 'TestInstallEgress|TestRemoveEgress' -v`
Expected: FAIL — `undefined: InstallEgressPolicy`, `undefined: RemoveEgressPolicy`.

- [ ] **Step 3: Add the executor helpers**

Append to `internal/firecracker/egress.go`:

```go
// InstallEgressPolicy applies a VM's egress filter on the Firecracker host.
//
// Call this BEFORE the microVM boots. Installing post-boot (as the old
// agent-driven path did) leaves a window in which a booting guest reaches the
// network before its policy lands.
//
// A failure to install a deny/allow policy is returned to the caller and must
// abort VM creation: a VM running with a silently-unapplied filter is strictly
// worse than one that failed to start. An open policy has nothing to enforce,
// so its teardown is best-effort.
func InstallEgressPolicy(exec Executor, alloc state.NetAllocation, policy state.ParsedNetPolicy) error {
	out, err := exec.Run(EgressInstallScript(alloc, policy))
	if policy.Mode == state.NetPolicyOpen {
		return nil
	}
	if err != nil {
		return fmt.Errorf("install egress policy on %s: %w (output: %s)", alloc.TAPDev, err, out)
	}
	return nil
}

// RemoveEgressPolicy tears down a VM's egress filter. Best-effort by design:
// it runs on shutdown paths that must complete regardless.
//
// This must run on every path that releases a TAP device, or the next VM
// allocated at this index inherits the stale chain.
func RemoveEgressPolicy(exec Executor, vm *state.VM) {
	exec.Run(EgressRemoveScript(state.AllocateNet(vm.NetIndex)))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/firecracker/ -run 'TestInstallEgress|TestRemoveEgress' -v`
Expected: PASS — four test functions.

- [ ] **Step 5: Delete the in-guest enforcement path**

In `internal/firecracker/process.go`, delete the whole `ApplyNetworkPolicyViaAgent` function (lines 165-188, from the `// ApplyNetworkPolicyViaAgent sets iptables rules via the agent.` comment through its closing brace).

In the same file, update the deprecation comment on `AgentExec` (~line 210) to drop the reference:

```go
// callers here (SetupGuestNetworkViaAgent, StopViaAgent, ApplySeccompViaAgent)
```

In `internal/server/routes.go`, delete these three lines from `postBootSetup`:

```go
	if err := firecracker.ApplyNetworkPolicyViaAgent(s.executor, postVM); err != nil {
		log.Printf("VM %s: network policy setup failed: %v", name, err)
	}
```

- [ ] **Step 6: Install pre-boot in `handleCreateVM`**

In `internal/server/routes.go`, immediately after `alloc := state.AllocateNet(netIndex)`, insert:

```go
	// Install the host-side egress filter before the microVM boots. The guest
	// runs as root and can flush any in-guest rule, so enforcement lives here
	// in Lima; doing it pre-boot also closes the window in which a booting
	// guest could reach the network before its policy landed.
	policy, err := state.ParseNetPolicy(req.NetPolicy)
	if err != nil {
		s.store.RemoveVM(req.Name)
		httpError(w, err, http.StatusBadRequest)
		return
	}
	if err := firecracker.InstallEgressPolicy(s.executor, alloc, policy); err != nil {
		s.store.RemoveVM(req.Name)
		httpError(w, err, http.StatusInternalServerError)
		return
	}
```

- [ ] **Step 7: Tear down on the stop and restore paths**

In `internal/server/routes.go`, add a teardown call next to each existing `firecracker.RemovePortForwarding(s.executor, vm)` — the stop handler (~line 679) and the restore handler (~line 830):

```go
	firecracker.RemovePortForwarding(s.executor, vm)
	firecracker.RemoveEgressPolicy(s.executor, vm)
```

- [ ] **Step 8: Verify the whole suite still builds and passes**

Run: `make test`
Expected: PASS. A compile error naming `ApplyNetworkPolicyViaAgent` means a caller was missed — remove it; the guest must never enforce its own policy.

- [ ] **Step 9: Manual acceptance test — this is the one that proves the fix**

This cannot be asserted in a unit test; run it against a real Lima VM.

Note `mvm exec` takes the command directly — there is no `--` separator, and
passing one makes the guest try to run `--` as the binary.

```bash
mvm start netcage --net-policy deny
# Expect failure — egress is blocked on the host. -4 is load-bearing: without
# it DNS returns AAAA, the connection dies for lack of a v6 route regardless
# of policy, and a broken filter would still look like it was working.
mvm exec netcage curl -4 -s -m 5 -o /dev/null -w "%{http_code}\n" https://example.com
# Now have the guest try to free itself, exactly as a rogue agent would:
mvm exec netcage iptables -F
mvm exec netcage ip6tables -F
mvm exec netcage curl -4 -s -m 5 -o /dev/null -w "%{http_code}\n" https://example.com
```

Expected: both print `000`. Before this change the second prints `200` — that is
the vulnerability, and it reproduces today on applevz.

Also confirm IPv6 is actually covered, since that is the half most likely to be
skipped:

```bash
limactl shell mvm sudo ip6tables -L MVM-EGRESS-0 -n
```

Expected: the chain exists and ends in DROP. An empty or missing chain means
the policy is IPv4-only and one routing change from being bypassed entirely.

Confirm the host-side chain is what is doing the work, and that teardown is clean:

```bash
limactl shell mvm sudo iptables -L MVM-EGRESS-0 -n -v
mvm delete netcage --force
limactl shell mvm sudo iptables -L MVM-EGRESS-0 -n -v   # expect: No chain/target/match by that name
```

- [ ] **Step 10: Commit**

```bash
git add internal/firecracker/egress.go internal/firecracker/egress_test.go \
        internal/firecracker/process.go internal/server/routes.go
git commit -m "fix(firecracker): enforce net-policy on the host, not inside the guest

The guest runs as root, so iptables rules installed inside it could be
flushed by the process they were meant to contain. Egress is now filtered
by a per-VM iptables chain in the Lima VM, installed before boot and torn
down with the TAP device.

deny no longer carves out DNS: a reachable resolver is an exfiltration
channel under a policy named deny."
```

---

### Task 4: DNS-driven allowlist

Replaces the one-shot `getent` lookup that pinned IPs at boot. A per-VM DNS proxy answers only allowlisted names and feeds each answer into the VM's ipset, so CDN rotation is handled automatically and a blocked name simply never resolves.

**Files:**
- Create: `internal/egressdns/resolver.go`
- Test: `internal/egressdns/resolver_test.go`
- Modify: `internal/server/routes.go` (start the proxy alongside the filter; stop it on teardown)
- Modify: `internal/firecracker/process.go:101-106` (`SetupGuestNetworkViaAgent` takes the resolver address)
- Modify: `go.mod` / `go.sum` (add `github.com/miekg/dns`)

**Dependency note for the implementer:** this adds `github.com/miekg/dns` to a project whose only direct dependencies are `cobra` and `menuet`. Hand-rolling DNS wire parsing in security-critical code is the worse option; `miekg/dns` is the de-facto standard and has no transitive dependencies beyond `golang.org/x/*`, which is already vendored. If the reviewer rejects the dependency, the fallback is to restrict the proxy to A/AAAA over UDP and parse the ~120 lines of wire format by hand — but do not start there.

**Interfaces:**
- Consumes: `state.NetAllocation`, `state.ParsedNetPolicy` (Task 1), `EgressIPSetName` (Task 2).
- Produces: `egressdns.Allowed(qname string, domains []string) bool`, `egressdns.Resolver` with `NewResolver(upstream string) *Resolver`, `(*Resolver).Start(alloc state.NetAllocation, sets SetPair, domains []string) error`, `(*Resolver).Stop(index int)`, and `egressdns.SetPair{V4, V6 string}`.

- [ ] **Step 1: Write the failing test**

The allowlist matcher is the security-critical unit. Create `internal/egressdns/resolver_test.go`:

```go
package egressdns

import "testing"

// TestAllowedMatchesOnLabelBoundaries is the load-bearing test in this
// package. A naive strings.HasSuffix match would let an attacker register
// evilgithub.com and reach it under an allowlist of github.com.
func TestAllowedMatchesOnLabelBoundaries(t *testing.T) {
	domains := []string{"github.com", "registry.npmjs.org"}

	allowed := []string{
		"github.com.",
		"github.com",
		"api.github.com.",
		"codeload.github.com.",
		"GitHub.COM.",
		"registry.npmjs.org.",
	}
	for _, q := range allowed {
		if !Allowed(q, domains) {
			t.Errorf("Allowed(%q) = false, want true", q)
		}
	}

	denied := []string{
		"evilgithub.com.",
		"github.com.evil.com.",
		"notgithub.com.",
		"github.como.",
		"npmjs.org.",         // allowlist named registry.npmjs.org, not the parent
		"xregistry.npmjs.org.",
		"",
		".",
	}
	for _, q := range denied {
		if Allowed(q, domains) {
			t.Errorf("Allowed(%q) = true, want false", q)
		}
	}
}

func TestAllowedWithEmptyAllowlistDeniesEverything(t *testing.T) {
	for _, q := range []string{"github.com.", "anything.example."} {
		if Allowed(q, nil) {
			t.Errorf("Allowed(%q) with no allowlist = true, want false", q)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/egressdns/ -run TestAllowed -v`
Expected: FAIL — package does not exist / `undefined: Allowed`.

- [ ] **Step 3: Implement the matcher**

Create `internal/egressdns/resolver.go` with the matcher first:

```go
// Package egressdns implements the DNS proxy backing mvm's allow:<domains>
// network policy.
//
// The policy's filter matches on an ipset that only this proxy writes to: a
// name resolves, and its addresses become reachable, only if the name is on
// the VM's allowlist. That inverts the old design, which resolved each domain
// once with getent at boot and pinned the resulting IPs — a scheme that broke
// open or closed unpredictably as CDNs rotated addresses.
package egressdns

import "strings"

// Allowed reports whether qname is covered by the allowlist. A domain covers
// itself and its subdomains, matched on label boundaries: "github.com" covers
// "api.github.com" but not "evilgithub.com".
func Allowed(qname string, domains []string) bool {
	q := strings.ToLower(strings.TrimSuffix(qname, "."))
	if q == "" {
		return false
	}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSuffix(d, "."))
		if d == "" {
			continue
		}
		if q == d || strings.HasSuffix(q, "."+d) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the matcher tests to verify they pass**

Run: `go test ./internal/egressdns/ -run TestAllowed -v`
Expected: PASS — two test functions.

- [ ] **Step 5: Commit the matcher**

```bash
git add internal/egressdns/resolver.go internal/egressdns/resolver_test.go
git commit -m "feat(egressdns): label-boundary allowlist matching"
```

- [ ] **Step 6: Write the failing test for the query handler**

In `internal/egressdns/resolver_test.go`, **replace** the existing `import "testing"` line with the block below (Go requires all import declarations to precede any other declaration, so this must replace the old one rather than be appended), then append the three test functions to the end of the file:

```go
import (
	"net"
	"testing"

	"github.com/miekg/dns"
)
```

```go
func TestHandlerRefusesNamesOffTheAllowlist(t *testing.T) {
	var upstreamCalls int
	r := &Resolver{
		domains: map[int][]string{0: {"github.com"}},
		sets:    map[int]SetPair{0: {V4: "mvm-allow-0", V6: "mvm-allow6-0"}},
		exchange: func(*dns.Msg, string) (*dns.Msg, error) {
			upstreamCalls++
			return nil, nil
		},
		ipsetAdd: func(string, net.IP, uint32) error { return nil },
	}

	q := new(dns.Msg)
	q.SetQuestion("evil.example.", dns.TypeA)
	resp := r.handle(0, q)

	if resp.Rcode != dns.RcodeRefused {
		t.Errorf("Rcode = %v, want REFUSED", dns.RcodeToString[resp.Rcode])
	}
	if upstreamCalls != 0 {
		t.Error("a refused name must never be forwarded upstream — that leaks the query itself")
	}
}

func TestHandlerPopulatesIPSetFromAnswers(t *testing.T) {
	type added struct {
		set string
		ip  net.IP
		ttl uint32
	}
	var got []added

	r := &Resolver{
		domains: map[int][]string{0: {"github.com"}},
		sets:    map[int]SetPair{0: {V4: "mvm-allow-0", V6: "mvm-allow6-0"}},
		exchange: func(m *dns.Msg, _ string) (*dns.Msg, error) {
			resp := new(dns.Msg)
			resp.SetReply(m)
			rr, _ := dns.NewRR("api.github.com. 30 IN A 140.82.114.6")
			resp.Answer = []dns.RR{rr}
			return resp, nil
		},
		ipsetAdd: func(set string, ip net.IP, ttl uint32) error {
			got = append(got, added{set, ip, ttl})
			return nil
		},
	}

	q := new(dns.Msg)
	q.SetQuestion("api.github.com.", dns.TypeA)
	resp := r.handle(0, q)

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("Rcode = %v, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 ipset add, got %d: %v", len(got), got)
	}
	if got[0].set != "mvm-allow-0" {
		t.Errorf("added to %q, want mvm-allow-0", got[0].set)
	}
	if !got[0].ip.Equal(net.ParseIP("140.82.114.6")) {
		t.Errorf("added IP %v, want 140.82.114.6", got[0].ip)
	}
	// A TTL shorter than the floor would expire the ipset entry before the
	// client finished using the address it was just handed.
	if got[0].ttl < minIPSetTTL {
		t.Errorf("ttl = %d, want at least the %d-second floor", got[0].ttl, minIPSetTTL)
	}
}

// TestHandlerRoutesAAAAToTheV6Set pins that an AAAA answer lands in the inet6
// set. Sending it to the inet set would fail at the ipset layer rather than
// store it, leaving every IPv6 address the proxy resolved unreachable — a
// failure that is invisible until a dual-stack host prefers the v6 route.
func TestHandlerRoutesAAAAToTheV6Set(t *testing.T) {
	var gotSets []string
	r := &Resolver{
		domains: map[int][]string{0: {"github.com"}},
		sets:    map[int]SetPair{0: {V4: "mvm-allow-0", V6: "mvm-allow6-0"}},
		exchange: func(m *dns.Msg, _ string) (*dns.Msg, error) {
			resp := new(dns.Msg)
			resp.SetReply(m)
			a, _ := dns.NewRR("github.com. 300 IN A 140.82.114.6")
			aaaa, _ := dns.NewRR("github.com. 300 IN AAAA 2606:50c0:8000::153")
			resp.Answer = []dns.RR{a, aaaa}
			return resp, nil
		},
		ipsetAdd: func(set string, _ net.IP, _ uint32) error {
			gotSets = append(gotSets, set)
			return nil
		},
	}
	q := new(dns.Msg)
	q.SetQuestion("github.com.", dns.TypeA)
	r.handle(0, q)

	if len(gotSets) != 2 {
		t.Fatalf("got %d ipset adds, want 2 (one per family): %v", len(gotSets), gotSets)
	}
	if gotSets[0] != "mvm-allow-0" {
		t.Errorf("A record went to %q, want mvm-allow-0", gotSets[0])
	}
	if gotSets[1] != "mvm-allow6-0" {
		t.Errorf("AAAA record went to %q, want mvm-allow6-0", gotSets[1])
	}
}

func TestHandlerRefusesUnknownVM(t *testing.T) {
	r := &Resolver{
		domains:  map[int][]string{},
		sets:     map[int]SetPair{},
		exchange: func(*dns.Msg, string) (*dns.Msg, error) { return nil, nil },
		ipsetAdd: func(string, net.IP, uint32) error { return nil },
	}
	q := new(dns.Msg)
	q.SetQuestion("github.com.", dns.TypeA)
	if resp := r.handle(99, q); resp.Rcode != dns.RcodeRefused {
		t.Errorf("Rcode = %v for an unregistered VM, want REFUSED", dns.RcodeToString[resp.Rcode])
	}
}
```

- [ ] **Step 7: Run to verify it fails**

Run: `go get github.com/miekg/dns && go test ./internal/egressdns/ -run TestHandler -v`
Expected: FAIL — `undefined: Resolver`, `undefined: minIPSetTTL`.

- [ ] **Step 8: Implement the resolver**

In `internal/egressdns/resolver.go`, **replace** the existing `import "strings"` line with the block below (same reason as Step 6 — import declarations must precede all other declarations), then append everything after it to the end of the file:

```go
import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/agentstep/mvm/internal/state"
	"github.com/miekg/dns"
)
```

```go
// minIPSetTTL floors how long a resolved address stays reachable. Some CDNs
// answer with a TTL of a few seconds; expiring the ipset entry that fast would
// break the connection the client is opening with the address it was just
// given.
const minIPSetTTL = 60

// SetPair is one VM's two allowlist ipsets. They are separate because an
// ipset's address family is fixed at creation: adding an IPv6 address to an
// inet set fails rather than storing it.
type SetPair struct {
	V4 string
	V6 string
}

// Resolver runs one DNS listener per allow-policy VM, bound to that VM's own
// gateway address. The listener a query arrives on identifies the VM, so no
// client-supplied field is ever trusted for authorization.
type Resolver struct {
	upstream string

	mu      sync.RWMutex
	domains map[int][]string      // net index -> allowlist
	sets    map[int]SetPair       // net index -> its family-locked ipsets
	servers map[int][]*dns.Server // net index -> its udp+tcp listeners

	// Injectable for tests.
	exchange func(*dns.Msg, string) (*dns.Msg, error)
	ipsetAdd func(set string, ip net.IP, ttl uint32) error
}

// NewResolver returns a Resolver forwarding to upstream (host:port).
func NewResolver(upstream string) *Resolver {
	c := new(dns.Client)
	return &Resolver{
		upstream: upstream,
		domains:  map[int][]string{},
		sets:     map[int]SetPair{},
		servers:  map[int][]*dns.Server{},
		exchange: func(m *dns.Msg, addr string) (*dns.Msg, error) {
			resp, _, err := c.Exchange(m, addr)
			return resp, err
		},
		ipsetAdd: runIPSetAdd,
	}
}

// Start binds UDP and TCP listeners on the VM's gateway address and registers
// its allowlist. Safe to call again for the same index: the previous listeners
// are stopped first, so a policy change converges.
func (r *Resolver) Start(alloc state.NetAllocation, sets SetPair, domains []string) error {
	r.Stop(alloc.Index)

	r.mu.Lock()
	r.domains[alloc.Index] = domains
	r.sets[alloc.Index] = sets
	r.mu.Unlock()

	addr := net.JoinHostPort(alloc.TAPIP, "53")
	index := alloc.Index
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		w.WriteMsg(r.handle(index, req))
	})

	var started []*dns.Server
	for _, netProto := range []string{"udp", "tcp"} {
		srv := &dns.Server{Addr: addr, Net: netProto, Handler: mux}
		ready := make(chan error, 1)
		srv.NotifyStartedFunc = func() { ready <- nil }
		go func() {
			if err := srv.ListenAndServe(); err != nil {
				select {
				case ready <- err:
				default:
				}
			}
		}()
		if err := <-ready; err != nil {
			for _, s := range started {
				s.Shutdown()
			}
			r.Stop(index)
			return fmt.Errorf("egress DNS listen on %s/%s: %w", addr, netProto, err)
		}
		started = append(started, srv)
	}

	r.mu.Lock()
	r.servers[index] = started
	r.mu.Unlock()
	return nil
}

// Stop shuts down a VM's listeners and forgets its allowlist. Idempotent.
func (r *Resolver) Stop(index int) {
	r.mu.Lock()
	servers := r.servers[index]
	delete(r.servers, index)
	delete(r.domains, index)
	delete(r.sets, index)
	r.mu.Unlock()

	for _, s := range servers {
		s.Shutdown()
	}
}

// handle answers one query on behalf of the VM at the given net index.
//
// A name off the allowlist is REFUSED without being forwarded: sending it
// upstream would leak the lookup itself, which is exactly the exfiltration
// channel the policy exists to close.
func (r *Resolver) handle(index int, req *dns.Msg) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(req)

	r.mu.RLock()
	domains, known := r.domains[index]
	sets := r.sets[index]
	r.mu.RUnlock()

	if !known || len(req.Question) == 0 {
		resp.Rcode = dns.RcodeRefused
		return resp
	}
	for _, q := range req.Question {
		if !Allowed(q.Name, domains) {
			resp.Rcode = dns.RcodeRefused
			return resp
		}
	}

	upstream, err := r.exchange(req, r.upstream)
	if err != nil || upstream == nil {
		resp.Rcode = dns.RcodeServerFailure
		return resp
	}

	for _, rr := range upstream.Answer {
		var ip net.IP
		// A and AAAA go to different sets: an ipset's family is fixed at
		// creation, and adding an IPv6 address to the inet set fails rather
		// than storing it — so a shared set would leave every AAAA the proxy
		// resolved unreachable, silently.
		target := sets.V4
		switch v := rr.(type) {
		case *dns.A:
			ip = v.A
		case *dns.AAAA:
			ip = v.AAAA
			target = sets.V6
		default:
			continue
		}
		ttl := rr.Header().Ttl
		if ttl < minIPSetTTL {
			ttl = minIPSetTTL
		}
		r.ipsetAdd(target, ip, ttl)
	}

	upstream.Id = req.Id
	return upstream
}

// runIPSetAdd authorizes one address for one VM. The set name comes from
// firecracker.EgressIPSetName and the IP from a parsed DNS record, so neither
// is user-controlled text; exec.Command is used without a shell regardless.
func runIPSetAdd(set string, ip net.IP, ttl uint32) error {
	if ip == nil {
		return nil
	}
	if strings.ContainsAny(set, " \t\n;|&$`") {
		return fmt.Errorf("refusing suspicious ipset name %q", set)
	}
	return exec.Command("sudo", "ipset", "add", set, ip.String(),
		"timeout", strconv.FormatUint(uint64(ttl), 10), "-exist").Run()
}
```

- [ ] **Step 9: Run to verify the handler tests pass**

Run: `go test ./internal/egressdns/ -v`
Expected: PASS — five test functions.

- [ ] **Step 10: Point allow-policy guests at their own gateway**

The guest currently hardcodes `nameserver 8.8.8.8` (`internal/firecracker/process.go:104`), which the Task 2 filter now drops under `allow:`. Change the signature to take the resolver address:

```go
// SetupGuestNetworkViaAgent configures the guest's default route and resolver.
// resolverIP is the gateway under an allow: policy (where internal/egressdns
// listens) and a public resolver otherwise.
func SetupGuestNetworkViaAgent(ex Executor, guestIP, gatewayIP, resolverIP string) error {
	return agentExec(ex, guestIP, fmt.Sprintf(
		"ip route add default via %s dev eth0 2>/dev/null; echo 'nameserver %s' > /etc/resolv.conf",
		gatewayIP, resolverIP))
}
```

In `internal/server/routes.go`, `postBootSetup` needs the policy to choose. Change its signature to accept `policy state.ParsedNetPolicy` and replace the call:

```go
	resolverIP := "8.8.8.8"
	if policy.Mode == state.NetPolicyAllow {
		resolverIP = alloc.TAPIP
	}
	firecracker.SetupGuestNetworkViaAgent(s.executor, alloc.GuestIP, alloc.TAPIP, resolverIP)
```

Change the signature at `internal/server/routes.go:229` to:

```go
func (s *Server) postBootSetup(name string, alloc state.NetAllocation, volumes []string, seccomp string, policy state.ParsedNetPolicy) error {
```

There are exactly two call sites:

- **`internal/server/routes.go:371`** — inside `handleCreateVM`, which already has `policy` in scope from Task 3:

```go
		return s.postBootSetup(req.Name, alloc, req.Volumes, req.Seccomp, policy)
```

- **`internal/server/routes.go:449`** — the resume path, which has no parsed policy in scope. Re-derive it from persisted state and **fail closed**: a policy string that no longer parses must not silently downgrade to open.

```go
	resumePolicy, err := state.ParseNetPolicy(vm.NetPolicy)
	if err != nil {
		log.Printf("VM %s: unparseable stored policy %q, failing closed to deny: %v", name, vm.NetPolicy, err)
		resumePolicy = state.ParsedNetPolicy{Mode: state.NetPolicyDeny}
	}
	postBoot := func() error { return s.postBootSetup(name, alloc, volumes, seccomp, resumePolicy) }
```

Read the surrounding function first to confirm the VM value is named `vm` at that point; if it is not yet loaded, fetch it with `s.store.GetVM(name)` before this block.

Fix the other caller at `internal/firecracker/install.go:229`, which passes the literal string; give it `"8.8.8.8"` as the resolver.

- [ ] **Step 11: Start and stop the proxy with the VM**

Add the field to the `Server` struct at `internal/server/server.go:28`, after `executor`:

```go
type Server struct {
	store        *state.Store
	executor     firecracker.Executor
	dns          *egressdns.Resolver
	unixListener net.Listener
	// ... remaining fields unchanged
}
```

Initialize it in `func New(cfg Config) (*Server, error)` at `internal/server/server.go:86`, wherever the struct is constructed:

```go
	dns: egressdns.NewResolver("8.8.8.8:53"),
```

Add `"github.com/agentstep/mvm/internal/egressdns"` to that file's imports.

In `handleCreateVM`, immediately after the `InstallEgressPolicy` call added in Task 3:

```go
	if policy.Mode == state.NetPolicyAllow {
		sets := egressdns.SetPair{
			V4: firecracker.EgressIPSetName(alloc.Index),
			V6: firecracker.EgressIPSetName6(alloc.Index),
		}
		if err := s.dns.Start(alloc, sets, policy.Domains); err != nil {
			firecracker.RemoveEgressPolicy(s.executor, vm)
			s.store.RemoveVM(req.Name)
			httpError(w, err, http.StatusInternalServerError)
			return
		}
	}
```

Alongside each `firecracker.RemoveEgressPolicy(s.executor, vm)` call from Task 3, add:

```go
	s.dns.Stop(vm.NetIndex)
```

- [ ] **Step 12: Verify the suite passes**

Run: `make test`
Expected: PASS.

- [ ] **Step 13: Manual acceptance test**

```bash
mvm start allowcage --net-policy allow:github.com
mvm exec allowcage -- curl -sI -m 10 https://github.com | head -1     # expect: HTTP/2 200
mvm exec allowcage -- curl -sI -m 10 https://example.com ; echo "exit: $?"  # expect: non-zero
# The lookup itself must be refused, not just the connection:
mvm exec allowcage -- getent hosts example.com ; echo "exit: $?"       # expect: non-zero
# And the guest must not be able to route around the proxy:
mvm exec allowcage -- curl -sI -m 10 --dns-servers 8.8.8.8 https://example.com ; echo "exit: $?"
limactl shell mvm sudo ipset list mvm-allow-0 | head -20               # expect: github.com's addresses
mvm delete allowcage --force
```

- [ ] **Step 14: Commit**

```bash
git add go.mod go.sum internal/egressdns/ internal/server/routes.go \
        internal/firecracker/process.go internal/firecracker/install.go
git commit -m "feat(egressdns): DNS-driven allowlist replaces boot-time IP pinning

allow:<domains> resolved each domain once with getent at boot and pinned
the resulting addresses, so CDN rotation broke the policy open or closed
unpredictably. A per-VM DNS proxy now answers only allowlisted names and
feeds each answer into the VM's ipset, and the filter permits DNS only to
that proxy."
```

---

### Task 5: Host dependencies, doctor check, and documentation

Nothing above works if `ipset` is missing from the Lima VM, and the VZ backend's remaining gap must be visible rather than implied.

**Files:**
- Modify: `internal/cli/init.go:180` (install `ipset` before NAT setup)
- Modify: `internal/cli/doctor.go`
- Create: `docs/networking.md`
- Test: `internal/state/netpolicy_test.go` (extend)

**Interfaces:**
- Consumes: `state.ParseNetPolicy` (Task 1), `firecracker.EgressChainName` (Task 2).
- Produces: `state.EgressHostPackages() []string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/state/netpolicy_test.go`:

```go
func TestEgressHostPackages(t *testing.T) {
	pkgs := EgressHostPackages()
	for _, want := range []string{"ipset", "iptables", "conntrack"} {
		found := false
		for _, p := range pkgs {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("EgressHostPackages() missing %q, got %v", want, pkgs)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/state/ -run TestEgressHostPackages -v`
Expected: FAIL — `undefined: EgressHostPackages`.

- [ ] **Step 3: Implement and wire the package install**

Append to `internal/state/netpolicy.go`:

```go
// EgressHostPackages lists what the Firecracker host (the Lima VM) needs for
// host-side egress enforcement. iptables is already required for NAT; ipset
// backs allow: policies and conntrack provides the ESTABLISHED,RELATED match.
func EgressHostPackages() []string {
	return []string{"iptables", "ipset", "conntrack"}
}
```

In `internal/cli/init.go`, immediately before the `state.SetupNATScript()` call at line 180:

```go
	installPkgs := fmt.Sprintf(
		"sudo apt-get update -qq && sudo apt-get install -y --no-install-recommends %s >/dev/null",
		strings.Join(state.EgressHostPackages(), " "))
	if _, err := limaClient.ShellScript(installPkgs); err != nil {
		return fmt.Errorf("install egress host packages: %w", err)
	}
```

Ensure `fmt` and `strings` are imported in that file.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/state/ -run TestEgressHostPackages -v`
Expected: PASS.

- [ ] **Step 5: Add the doctor checks**

In `internal/cli/doctor.go`, inside `runDoctor`, insert this immediately before the `fmt.Println()` that precedes the final `if issues == 0` summary (~line 154). It follows the surrounding conventions: two-space-indented output, `issues++` to record a failure, and `limaClient.Shell` for in-Lima checks. `backend` is already in scope from line 20.

```go
	// 13. Egress enforcement prerequisites. A missing ipset silently degrades
	// allow:<domains> to a total block, so surface it rather than letting a VM
	// start with a filter that can never match.
	if backend == "firecracker" {
		if _, err := limaClient.Shell("command -v ipset >/dev/null && sudo ipset list >/dev/null"); err != nil {
			fmt.Println("  ✗ ipset unavailable in the Lima VM — allow:<domains> policies will block all traffic")
			fmt.Println("    fix: mvm init --backend firecracker (re-runs the host package install)")
			issues++
		} else {
			fmt.Println("  ✓ ipset available (allow:<domains> egress policies)")
		}
	}

	if backend == "applevz" {
		fmt.Println("  ! applevz: --net-policy is enforced inside the guest, so a root process")
		fmt.Println("    in the sandbox can remove it. Use --backend firecracker where the")
		fmt.Println("    policy is a security boundary rather than a guardrail.")
	}
```

- [ ] **Step 6: Verify doctor builds and runs**

Run: `go build ./... && go run ./cmd/mvm system status`
Expected: builds; the diagnostics print the new ipset line. (`runDoctor` is wired to `mvm system status` at `internal/cli/system.go:189`, not to a top-level `doctor` command.)

- [ ] **Step 7: Write the documentation**

Create `docs/networking.md`:

```markdown
# Network policy

`--net-policy` controls what a sandbox can reach.

| Policy | Effect |
|---|---|
| `open` (default) | No filter. |
| `deny` | Every packet the guest originates is dropped, **including DNS**. |
| `allow:a.com,b.com` | Only addresses resolved for the listed domains and their subdomains are reachable. |

## Where enforcement happens

On the **Firecracker** backend, policy is enforced by a per-VM iptables chain
(`MVM-EGRESS-<index>`) in the Lima VM, outside the sandbox. The chain is
installed before the microVM boots and torn down with its TAP device.

This is the point: the guest runs as root. Any rule installed *inside* it can
be flushed by the very process it is meant to contain. Rules on the host
cannot.

`mvm exec` keeps working under every policy — vsock is not IP and never
traverses these chains. The operator keeps control; the guest loses the
network.

Published ports (`-p 3000:3000`) keep working under `deny`. Only traffic
arriving on the VM's TAP device is filtered; DNAT'd inbound connections arrive
on another interface.

## allow: and DNS

Each allow-policy VM gets a DNS proxy bound to its own gateway address, and
the filter permits port 53 only to that address. A name off the allowlist is
refused without being forwarded upstream — forwarding it would leak the lookup
itself. Every address the proxy returns is added to the VM's ipset with the
record's TTL (floored at 60s), so CDN rotation is handled automatically.

An earlier implementation resolved each domain once with `getent` at boot and
pinned the resulting IPs. That broke open (a rotated-away address still
allowed) and closed (the current address blocked) unpredictably.

## IPv6

Policies are installed into `ip6tables` as well as `iptables`, with a separate
`inet6` ipset for allowlists (an ipset's address family is fixed at creation,
and adding an IPv6 address to an `inet` set fails rather than storing it).

This matters more than the current setup suggests. The guest has no IPv6 route
today, so v6 egress fails on its own — but guest DNS already returns AAAA
records, so `curl` without `-4` prefers a v6 path. A filter that holds only
because the transport happens to be unconfigured is not a filter, and one
routing change would silently open every policy.

## Apple VZ

**On the `applevz` backend `--net-policy` is still enforced inside the guest
and is a guardrail, not a boundary.** There is no host-side TAP device to
filter on. Use `--backend firecracker` where the policy needs to hold against
the code running in the sandbox.

This is demonstrable, not theoretical — verified on applevz, 2026-08-19:

    mvm start denytest2 --net-policy deny
    mvm exec denytest2 sh -c 'curl -4 -s -o /dev/null -w "%{http_code}" https://example.com'
    # -> 000  (blocked)
    mvm exec denytest2 sh -c 'iptables -F OUTPUT'
    mvm exec denytest2 sh -c 'curl -4 -s -o /dev/null -w "%{http_code}" https://example.com'
    # -> 200  (the guest removed its own cage)

Force IPv4 when testing this. Without `-4`, DNS returns AAAA records and the
connection fails for lack of a v6 route regardless of policy, which reads as
"still blocked" and hides the escape.
```

- [ ] **Step 8: Run the full suite**

Run: `make test`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/state/netpolicy.go internal/state/netpolicy_test.go \
        internal/cli/init.go internal/cli/doctor.go docs/networking.md
git commit -m "feat(cli): egress host dependencies, doctor checks, and networking docs"
```

---

## Out of scope — needs its own plan

- **Apple VZ host-side enforcement.** No host TAP device exists, so the FORWARD-chain approach does not transfer. Likely needs a filtering resolver plus a proxy the guest is forced through. Task 5 makes the gap visible in `doctor` and the docs; it does not close it.
- **The `deny`-means-deny behavior change** may break existing users who relied on DNS working under `deny`. The change is deliberate and tested (`TestEgressDenyHasNoAcceptAtAll`), but it belongs in release notes.
- Items 2–6 from the Sprites review (in-guest checkpoint CLI, sandbox self-documentation, service manager, filesystem API, policy triad) are independent subsystems and each want a separate plan.
