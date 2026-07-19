# Host-Side Net-Policy Enforcement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move `--net-policy` (`open` | `deny` | `allow:d1,d2`) enforcement from *inside the guest* (iptables applied through the guest agent, which a root-in-guest process can flush and escape) to *the host*, where the sandboxed guest cannot reach it. Firecracker VMs get FORWARD-chain iptables rules keyed on their TAP device inside the Lima VM; applevz VMs get macOS `pf` anchor rules keyed on their DHCP-assigned IP. The CLI surface does not change.

**Architecture:** One pure, backend-neutral rule-generation core (`internal/netpolicy`, compiles and unit-tests on Linux CI) produces the exact iptables/pf rule text from a policy string plus resolved IPs. Each backend has a thin apply/remove layer that shells the generated text out through the privilege path it already uses: the Firecracker path reuses the in-Lima `Executor` (`sudo iptables`, identical to `SetupPortForwarding`); the applevz path shells `sudo pfctl -a mvm/<name>` non-interactively via a one-time `sudo mvm net-setup` installer (sudoers.d drop-in + a `pf.conf` anchor). Guest-side enforcement stays in place as defense-in-depth (belt-and-braces), re-labelled but not removed.

**Tech Stack:** Go 1.22+, cobra, stdlib-only tests (no testify) — matches `internal/cli`/`internal/firecracker` conventions. macOS `pf`/`pfctl` for applevz; Linux `iptables` inside Lima for Firecracker. Repo module path `github.com/agentstep/mvm`; run all commands from `/Users/paulmeller/Projects/firecracker`.

## Global Constraints

- **The CLI surface must not change.** `--net-policy open|deny|allow:...` keeps its exact spelling, defaults (`open`), and observable behavior. `internal/cli/start.go`'s flag definition (line 74) and `runStart`'s signature are untouched. Gateway depends on `mvm start <name> --net-policy deny` behaving observably the same (outbound blocked, DNS still resolvable) — the only change is *where* the block is enforced.
- **Guest-side enforcement is KEPT as defense-in-depth, not removed.** DECISION (belt-and-braces, justified in Task 7): `ApplyNetworkPolicyViaAgent` (`internal/firecracker/process.go:166`) and `applyVZNetworkPolicy` (`internal/cli/start.go:509`) continue to run. Removing them would trade a redundant-but-harmless second layer for a strictly weaker posture during the rollout; we keep both layers now and can drop the guest layer in a later, separately-verified change. Their doc comments are updated to say "defense-in-depth; the authoritative block is host-side."
- **The host-side layer is authoritative; the guest-side layer is advisory.** A verification failure (host-side rule not applied) is an error surfaced in logs; a guest-side failure remains a warning as today.
- **DNS strategy is resolve-at-apply-time (DECISION, Task 1/§Design decisions).** `allow:` domains are resolved to IPs on the host at the moment the policy is applied, and those IPs are baked into the ruleset. This mirrors exactly what the current guest-side code already does (`getent hosts <domain>` at apply time — `process.go:180`, `start.go:530`), so it is not a regression, and it avoids building a full DNS-intercepting proxy for v1. Trade-off and future path documented in §Design decisions.
- **No new privileged daemon on macOS.** The applevz path stays unprivileged (`mvm-vz` runs as the normal user — `VZNATNetworkDeviceAttachment` needs no root; see Findings). Privilege for `pf` comes from a documented one-time `sudo mvm net-setup`, never from making `mvm-vz` setuid or prompting mid-boot.
- Match existing code style: tabs, stdlib-only tests, no testify. Rule-generation functions are pure (string in, string out) and fully unit-tested; anything that shells out to a live backend is covered by documented manual verification, not unit tests.

---

## Current Enforcement (investigation findings — read now, from this codebase)

These are the facts the plan is built on, cited to file:line as of this branch (`fix/applevz-networking`).

**Where policy is applied today, per backend:**

- **Firecracker (daemon path):** `internal/server/routes.go:161` `handleCreateVM` spawns a post-boot goroutine (`routes.go:242-272`). After `WaitForGuest` + `SetupGuestNetworkViaAgent`, it calls `firecracker.ApplyNetworkPolicyViaAgent(s.executor, postVM)` at `routes.go:259`. That function (`internal/firecracker/process.go:166-189`) builds an **iptables OUTPUT-chain** ruleset (`process.go:173` for `deny`; `process.go:174-183` for `allow:`, using `getent hosts <domain>` to expand each domain at apply time) and runs it **inside the guest** via `agentExec` (`process.go:188`, `217`) — i.e. the rules live in the guest's own netns and a root process in the guest can `iptables -F` them.
- **applevz (local CLI path):** `internal/cli/start.go:405` `runStartAppleVZ` calls `applyVZNetworkPolicy(ctx, agent, netPolicy)`. That function (`start.go:509-539`) builds the *same* iptables OUTPUT-chain ruleset (`start.go:516` deny; `start.go:521-533` allow, again `getent hosts` at apply time) and runs it **inside the guest** via `agent.Exec` (`start.go:537`). The code comment at `start.go:507-508` already flags this: *"a follow-up will move both to a host-side packet filter."* This plan is that follow-up.

**Privilege paths that already exist (what host-side enforcement can reuse):**

- **Firecracker/Lima:** everything privileged runs via the `Executor` interface (`internal/firecracker/executor.go:17-20`, `Run`/`RunWithTimeout`), which is either `lima.Client` (macOS → `limactl shell`) or `LocalExecutor` (daemon inside Lima). Privileged host-side networking is already done this exact way: `SetupPortForwarding` (`internal/firecracker/network.go:12-39`) emits literal `sudo iptables -t nat -A PREROUTING …` strings and runs them through `exec.Run`. **Host-side net-policy for Firecracker is the same pattern with `sudo iptables -A FORWARD -i <tap> …`.** Teardown mirror: `RemovePortForwarding` (`network.go:42-55`).
- **applevz:** there is **no existing privileged host path on macOS.** `mvm-vz` is launched with a plain `exec.Command(b.binary, …)` (`internal/vm/applevz.go:152`) as the normal user; the entitlements file (`vz/mvm-vz.entitlements:9-10`) confirms `VZNATNetworkDeviceAttachment` needs **no** privileged entitlement and no root. The only `sudo` on the macOS side today is `mvm update` (`internal/cli/update.go:143`) and `mvm logs` (`internal/cli/logs.go:92`), both interactive one-offs — nothing mvm can reuse to program `pf`. **A new one-time privilege mechanism is required (Task 4: `mvm net-setup`).**

**applevz networking shape (what the pf rules key on):**

- `mvm-vz` attaches `VZNATNetworkDeviceAttachment` (`vz/Sources/mvm-vz/VM/VMManager.swift:72-77`) — Apple's built-in vmnet NAT. The guest gets its address by kernel DHCP (`ip=dhcp` in bootArgs, `start.go:305`) from Apple's NAT pool (observed `192.168.65.0/24`, gateway `192.168.65.1`; the subnet is Apple's to pick — `start.go:384-386`). The host does not know the guest IP until the guest agent self-reports it via `NetInfo` (`agent/internal/handler/network.go:36-46`), which `runStartAppleVZ` reads at `start.go:387-402` and stores in `vm.GuestIP`. **That `vm.GuestIP` is the key for the pf source-address match** — it is known only *after* agent-ready + NetInfo, so pf apply must happen at `start.go` right after that discovery block (after line 402, before/around the existing `applyVZNetworkPolicy` call at line 405).

**Contradictions vs. the workstream description (things the plan corrects):**

1. The workstream says applevz policy is applied "through the guest agent — see `ApplyNetworkPolicyViaAgent`." **Correct function, wrong location:** applevz does **not** call `ApplyNetworkPolicyViaAgent`; that function is Firecracker-only (`process.go`). applevz has its **own** near-duplicate, `applyVZNetworkPolicy` in `internal/cli/start.go:509`. Both must be handled.
2. The workstream says "`handleCreateVM`'s post-boot goroutine applies policy on the daemon path" — **confirmed accurate** (`routes.go:242-272`, call at `:259`).
3. The workstream implies mvm might already have a macOS privileged path ("does the vz helper run privileged? does anything use sudo?"). **Investigated: no.** `mvm-vz` is unprivileged and no reusable macOS root path exists — hence the new `mvm net-setup` installer.
4. Firecracker host-side is described as "FORWARD chain rules keyed on the TAP device." **Confirmed viable:** `vm.TAPDevice` is populated (`routes.go:235`, from `state.AllocateNet`) and the Lima executor already runs `sudo iptables`. One nuance the plan bakes in: the rules must sit in a **per-VM dedicated chain** jumped from FORWARD so teardown is a clean flush-and-delete (the existing port-forward code deletes exact rules, which is fine for 2 rules but brittle for a variable-length allow-list).

### Design decisions (made here, not deferred)

- **Privilege mechanism for pf (applevz):** a **one-time `sudo mvm net-setup`** that (a) installs `/etc/sudoers.d/mvm-pf` granting the invoking user passwordless `pfctl` for *only* the `mvm/*` anchor operations, and (b) idempotently appends a filter `anchor "mvm/*"` to `/etc/pf.conf` and enables pf. At runtime `mvm` populates per-VM sub-anchors with `sudo pfctl -a "mvm/<name>" -f -` (rules on stdin) — non-interactive, no mid-boot password prompt. This is the same shape Tailscale/Docker-Desktop/cloudflared use for macOS `pf`, and mirrors the Lima `sudo iptables` model. Passwordless (not interactive) is deliberate: policy is applied inside `runStartAppleVZ`'s post-boot flow, which must not block on a TTY.
- **DNS strategy for allow-lists:** **resolve-at-apply-time.** `allow:d1,d2` domains are resolved with `net.LookupHost` on the host when the policy is applied; the resulting IPs are written into the ruleset. Trade-off: IPs baked at apply time go **stale** if the domain re-resolves to new addresses (CDNs, DNS round-robin) later in the VM's life — the guest could then be denied a now-valid IP or (less likely) briefly allowed a recycled one. Accepted for v1 because it (a) exactly matches today's guest-side `getent hosts`-at-apply behavior, so it is not a regression, and (b) avoids building a DNS-intercepting proxy. **Future path (out of scope, noted for the follow-up):** a local DNS resolver on the host that pins A-record answers for allow-listed domains into a live pf `table`/ipset, refreshed on each lookup — this is the only way to make allow-lists robust against re-resolution and is a substantially larger build.

---

### Task 1: Pure rule-generation core — `internal/netpolicy`

**Files:**
- Create: `internal/netpolicy/netpolicy.go`
- Test: `internal/netpolicy/netpolicy_test.go`

**Interfaces:**
- Produces:
  - `func Resolve(policy string, lookup func(string) ([]string, error)) ([]string, error)` — for `allow:` returns the sorted, de-duped IPv4 addresses of every listed domain; for `open`/`deny` returns `nil`. `lookup` is injected for testability (production passes `net.LookupHost`).
  - `func IptablesForwardRules(chain, tapDev, policy string, allowedIPs []string) ([]string, error)` — the lines to populate a per-VM FORWARD sub-chain (Firecracker/Lima). Each element is the argument string after `iptables` (no `sudo`, no chain create/jump — the apply layer adds those).
  - `func PFAnchorRules(guestIP, policy string, allowedIPs []string) (string, error)` — the full pf anchor body for one applevz VM (deny/allow), or `""` for `open`.
- Consumed by: Task 2 (Firecracker apply) and Task 5 (applevz apply).

- [ ] **Step 1: Write the failing test**

Create `internal/netpolicy/netpolicy_test.go`:

```go
package netpolicy

import (
	"fmt"
	"strings"
	"testing"
)

// === Resolve ===

func fakeLookup(m map[string][]string) func(string) ([]string, error) {
	return func(host string) ([]string, error) {
		ips, ok := m[host]
		if !ok {
			return nil, fmt.Errorf("no such host: %s", host)
		}
		return ips, nil
	}
}

func TestResolveOpenAndDenyReturnNil(t *testing.T) {
	for _, p := range []string{"open", "", "deny"} {
		got, err := Resolve(p, fakeLookup(nil))
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v", p, err)
		}
		if got != nil {
			t.Errorf("Resolve(%q) = %v, want nil", p, got)
		}
	}
}

func TestResolveAllowDedupesAndSortsIPv4(t *testing.T) {
	lookup := fakeLookup(map[string][]string{
		"github.com": {"140.82.112.3", "::1", "140.82.112.4"},
		"npmjs.org":  {"140.82.112.3", "104.16.0.1"},
	})
	got, err := Resolve("allow:github.com, npmjs.org", lookup)
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	want := []string{"104.16.0.1", "140.82.112.3", "140.82.112.4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Resolve = %v, want %v (sorted, de-duped, IPv4-only)", got, want)
	}
}

func TestResolveAllowSkipsUnresolvableDomains(t *testing.T) {
	lookup := fakeLookup(map[string][]string{"github.com": {"140.82.112.3"}})
	got, err := Resolve("allow:github.com,doesnotexist.invalid", lookup)
	if err != nil {
		t.Fatalf("Resolve should not fail on a single bad domain, got %v", err)
	}
	if len(got) != 1 || got[0] != "140.82.112.3" {
		t.Errorf("Resolve = %v, want just the resolvable IP", got)
	}
}

// === IptablesForwardRules ===

func TestIptablesForwardRulesDeny(t *testing.T) {
	lines, err := IptablesForwardRules("MVM-tap0", "tap0", "deny", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "-A MVM-tap0 -m state --state ESTABLISHED,RELATED -j ACCEPT") {
		t.Errorf("deny ruleset missing established-accept:\n%s", joined)
	}
	if !strings.Contains(joined, "--dport 53 -j ACCEPT") {
		t.Errorf("deny ruleset must still permit DNS:\n%s", joined)
	}
	if !strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), "-A MVM-tap0 -j DROP") {
		t.Errorf("deny ruleset must end in DROP, last line = %q", lines[len(lines)-1])
	}
}

func TestIptablesForwardRulesAllowInsertsIPsBeforeDrop(t *testing.T) {
	lines, err := IptablesForwardRules("MVM-tap0", "tap0", "allow:x", []string{"1.2.3.4", "5.6.7.8"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "-A MVM-tap0 -d 1.2.3.4 -j ACCEPT") ||
		!strings.Contains(joined, "-A MVM-tap0 -d 5.6.7.8 -j ACCEPT") {
		t.Errorf("allow ruleset missing per-IP accepts:\n%s", joined)
	}
	dropIdx := strings.Index(joined, "-j DROP")
	ipIdx := strings.Index(joined, "-d 5.6.7.8")
	if ipIdx == -1 || dropIdx == -1 || ipIdx > dropIdx {
		t.Errorf("per-IP accepts must precede the final DROP:\n%s", joined)
	}
}

func TestIptablesForwardRulesOpenIsEmpty(t *testing.T) {
	lines, err := IptablesForwardRules("MVM-tap0", "tap0", "open", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("open policy = %v, want no rules", lines)
	}
}

func TestIptablesForwardRulesRejectsUnknownPolicy(t *testing.T) {
	if _, err := IptablesForwardRules("MVM-tap0", "tap0", "bogus", nil); err == nil {
		t.Fatal("want error for unknown policy")
	}
}

// === PFAnchorRules ===

func TestPFAnchorRulesDeny(t *testing.T) {
	body, err := PFAnchorRules("192.168.65.4", "deny", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(body, "port 53") {
		t.Errorf("deny anchor must permit DNS:\n%s", body)
	}
	if !strings.Contains(body, "block drop quick from 192.168.65.4 to any") {
		t.Errorf("deny anchor must block the guest IP outbound:\n%s", body)
	}
}

func TestPFAnchorRulesAllow(t *testing.T) {
	body, err := PFAnchorRules("192.168.65.4", "allow:x", []string{"1.2.3.4"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(body, "pass quick from 192.168.65.4 to 1.2.3.4") {
		t.Errorf("allow anchor must pass to the resolved IP:\n%s", body)
	}
	passIdx := strings.Index(body, "pass quick from 192.168.65.4 to 1.2.3.4")
	blockIdx := strings.Index(body, "block drop quick from 192.168.65.4 to any")
	if passIdx == -1 || blockIdx == -1 || passIdx > blockIdx {
		t.Errorf("pass rules must precede the block (first-match quick):\n%s", body)
	}
}

func TestPFAnchorRulesOpenIsEmpty(t *testing.T) {
	body, err := PFAnchorRules("192.168.65.4", "open", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if body != "" {
		t.Errorf("open policy body = %q, want empty", body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netpolicy/ -v`
Expected: FAIL — compile errors `undefined: Resolve`, `undefined: IptablesForwardRules`, `undefined: PFAnchorRules` (package doesn't exist yet).

- [ ] **Step 3: Write minimal implementation**

Create `internal/netpolicy/netpolicy.go`:

```go
// Package netpolicy generates the exact host-side firewall rule text that
// enforces an mvm --net-policy (open | deny | allow:d1,d2). It is pure and
// backend-neutral: the Firecracker path feeds IptablesForwardRules to the
// in-Lima executor (sudo iptables), the applevz path feeds PFAnchorRules to
// sudo pfctl. No I/O, no OS calls — so it compiles and unit-tests anywhere,
// including Linux CI.
package netpolicy

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// Resolve turns an allow: policy's domains into the sorted, de-duplicated set
// of IPv4 addresses to permit, resolving each domain via lookup (production
// passes net.LookupHost). open/deny resolve to nil. A single domain that fails
// to resolve is skipped, not fatal — matching the guest-side getent behavior
// this replaces (a typo'd domain simply contributes no allowed IPs).
//
// This is resolve-at-apply-time: the returned IPs are a snapshot. See the plan's
// Design-decisions section for the staleness trade-off and the DNS-proxy future
// path.
func Resolve(policy string, lookup func(string) ([]string, error)) ([]string, error) {
	if !strings.HasPrefix(policy, "allow:") {
		if policy == "open" || policy == "" || policy == "deny" {
			return nil, nil
		}
		return nil, fmt.Errorf("unknown network policy: %q", policy)
	}
	seen := map[string]bool{}
	for _, domain := range strings.Split(strings.TrimPrefix(policy, "allow:"), ",") {
		domain = strings.TrimSpace(domain)
		if domain == "" {
			continue
		}
		addrs, err := lookup(domain)
		if err != nil {
			continue // skip unresolvable domains, same as guest-side getent
		}
		for _, a := range addrs {
			ip := net.ParseIP(a)
			if ip == nil || ip.To4() == nil {
				continue // IPv4 only for v1
			}
			seen[ip.String()] = true
		}
	}
	ips := make([]string, 0, len(seen))
	for ip := range seen {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return ips, nil
}

// IptablesForwardRules returns the lines that populate a per-VM FORWARD
// sub-chain in the Lima VM. Each element is the text after "iptables" — the
// apply layer prefixes "sudo iptables " and creates/jumps the chain. Keying on
// -i <tapDev> matches traffic FROM the guest; established/related replies (incl.
// host->guest port-forward return traffic) and DNS stay permitted. open -> no
// lines.
func IptablesForwardRules(chain, tapDev, policy string, allowedIPs []string) ([]string, error) {
	if policy == "open" || policy == "" {
		return nil, nil
	}
	if policy != "deny" && !strings.HasPrefix(policy, "allow:") {
		return nil, fmt.Errorf("unknown network policy: %q", policy)
	}
	lines := []string{
		fmt.Sprintf("-A %s -m state --state ESTABLISHED,RELATED -j ACCEPT", chain),
		fmt.Sprintf("-A %s -p udp --dport 53 -j ACCEPT", chain),
		fmt.Sprintf("-A %s -p tcp --dport 53 -j ACCEPT", chain),
	}
	if strings.HasPrefix(policy, "allow:") {
		for _, ip := range allowedIPs {
			lines = append(lines, fmt.Sprintf("-A %s -d %s -j ACCEPT", chain, ip))
		}
	}
	lines = append(lines, fmt.Sprintf("-A %s -j DROP", chain))
	return lines, nil
}

// PFAnchorRules returns the pf anchor body enforcing policy for one applevz VM,
// keyed on its DHCP-assigned guestIP. pf is first-match-wins with `quick`, so
// pass rules (DNS, then allow-listed IPs) precede the final block. open -> "".
func PFAnchorRules(guestIP, policy string, allowedIPs []string) (string, error) {
	if policy == "open" || policy == "" {
		return "", nil
	}
	if policy != "deny" && !strings.HasPrefix(policy, "allow:") {
		return "", fmt.Errorf("unknown network policy: %q", policy)
	}
	var b strings.Builder
	// DNS always permitted so name resolution keeps working under deny/allow.
	fmt.Fprintf(&b, "pass out quick inet proto { tcp udp } from %s to any port 53\n", guestIP)
	if strings.HasPrefix(policy, "allow:") {
		for _, ip := range allowedIPs {
			fmt.Fprintf(&b, "pass quick from %s to %s\n", guestIP, ip)
		}
	}
	fmt.Fprintf(&b, "block drop quick from %s to any\n", guestIP)
	return b.String(), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/netpolicy/ -v`
Expected: PASS (all cases).

- [ ] **Step 5: Commit**

```bash
git add internal/netpolicy/netpolicy.go internal/netpolicy/netpolicy_test.go
git commit -m "feat(netpolicy): pure host-side rule generator for iptables and pf"
```

---

### Task 2: Firecracker/Lima host-side apply + remove (via the in-Lima executor)

**Files:**
- Create: `internal/firecracker/netpolicy.go`
- Test: `internal/firecracker/netpolicy_test.go`
- Modify: `internal/server/routes.go` (wire apply into the post-boot goroutine; wire remove into stop/delete)

**Interfaces:**
- Consumes: `netpolicy.Resolve`, `netpolicy.IptablesForwardRules` (Task 1); `Executor` (`internal/firecracker/executor.go:17`); `state.VM.TAPDevice`, `state.VM.NetPolicy`.
- Produces:
  - `func hostPolicyChain(tapDev string) string` — the per-VM chain name (`"MVM-"+tapDev`, iptables-legal, ≤28 chars since tap names are short).
  - `func ApplyNetworkPolicyHost(ex Executor, vm *state.VM) error` — resolves domains (host-side `net.LookupHost`), creates/flushes the chain, populates it, and jumps FORWARD to it. No-op for `open`.
  - `func RemoveNetworkPolicyHost(ex Executor, vm *state.VM)` — best-effort teardown mirror (unlink the FORWARD jump, flush + delete the chain).

- [ ] **Step 1: Write the failing test**

Create `internal/firecracker/netpolicy_test.go`:

```go
package firecracker

import (
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// recordExecutor captures every command string it is asked to Run.
type recordExecutor struct{ cmds []string }

func (r *recordExecutor) Run(command string) (string, error) {
	r.cmds = append(r.cmds, command)
	return "", nil
}
func (r *recordExecutor) RunWithTimeout(command string, _ time.Duration) (string, error) {
	r.cmds = append(r.cmds, command)
	return "", nil
}

func TestHostPolicyChainDerivedFromTap(t *testing.T) {
	if got := hostPolicyChain("mvmtap3"); got != "MVM-mvmtap3" {
		t.Errorf("hostPolicyChain = %q, want MVM-mvmtap3", got)
	}
}

func TestApplyNetworkPolicyHostOpenIsNoop(t *testing.T) {
	ex := &recordExecutor{}
	vm := &state.VM{TAPDevice: "mvmtap0", NetPolicy: "open"}
	if err := ApplyNetworkPolicyHost(ex, vm); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(ex.cmds) != 0 {
		t.Errorf("open policy should run no commands, ran: %v", ex.cmds)
	}
}

func TestApplyNetworkPolicyHostDenyUsesForwardChainAndSudo(t *testing.T) {
	ex := &recordExecutor{}
	vm := &state.VM{TAPDevice: "mvmtap0", NetPolicy: "deny"}
	if err := ApplyNetworkPolicyHost(ex, vm); err != nil {
		t.Fatalf("err = %v", err)
	}
	all := strings.Join(ex.cmds, "\n")
	if !strings.Contains(all, "sudo iptables") {
		t.Errorf("must shell sudo iptables via the executor:\n%s", all)
	}
	if !strings.Contains(all, "FORWARD -i mvmtap0 -j MVM-mvmtap0") {
		t.Errorf("must jump FORWARD -> per-VM chain keyed on the tap:\n%s", all)
	}
	if !strings.Contains(all, "-A MVM-mvmtap0 -j DROP") {
		t.Errorf("deny ruleset must end in DROP inside the chain:\n%s", all)
	}
}

func TestRemoveNetworkPolicyHostTearsDownChain(t *testing.T) {
	ex := &recordExecutor{}
	vm := &state.VM{TAPDevice: "mvmtap0", NetPolicy: "deny"}
	RemoveNetworkPolicyHost(ex, vm)
	all := strings.Join(ex.cmds, "\n")
	if !strings.Contains(all, "-D FORWARD -i mvmtap0 -j MVM-mvmtap0") {
		t.Errorf("must remove the FORWARD jump:\n%s", all)
	}
	if !strings.Contains(all, "-X MVM-mvmtap0") {
		t.Errorf("must delete the per-VM chain:\n%s", all)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/firecracker/ -run 'HostPolicy|NetworkPolicyHost' -v`
Expected: FAIL — `undefined: hostPolicyChain`, `undefined: ApplyNetworkPolicyHost`, `undefined: RemoveNetworkPolicyHost`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/firecracker/netpolicy.go`:

```go
package firecracker

import (
	"fmt"
	"net"
	"strings"

	"github.com/agentstep/mvm/internal/netpolicy"
	"github.com/agentstep/mvm/internal/state"
)

// hostPolicyChain is the per-VM iptables chain that holds a VM's net-policy
// rules inside the Lima VM. Keyed on the tap device (short, unique per VM) so
// teardown is a clean flush+delete. Tap names are ~7 chars, well under
// iptables' 28-char chain-name limit.
func hostPolicyChain(tapDev string) string { return "MVM-" + tapDev }

// ApplyNetworkPolicyHost enforces vm.NetPolicy on the HOST side (inside the
// Lima VM), where the guest cannot reach it: FORWARD-chain iptables rules keyed
// on the VM's tap device. This is the authoritative enforcement; the guest-side
// ApplyNetworkPolicyViaAgent remains as defense-in-depth. No-op for open.
//
// Domains in an allow: policy are resolved host-side at apply time (see the
// plan's DNS design decision).
func ApplyNetworkPolicyHost(ex Executor, vm *state.VM) error {
	if vm.NetPolicy == "" || vm.NetPolicy == "open" {
		return nil
	}
	if vm.TAPDevice == "" {
		return fmt.Errorf("apply host net-policy: VM %q has no tap device", vm.Name)
	}

	allowedIPs, err := netpolicy.Resolve(vm.NetPolicy, net.LookupHost)
	if err != nil {
		return fmt.Errorf("apply host net-policy: %w", err)
	}
	chain := hostPolicyChain(vm.TAPDevice)
	lines, err := netpolicy.IptablesForwardRules(chain, vm.TAPDevice, vm.NetPolicy, allowedIPs)
	if err != nil {
		return fmt.Errorf("apply host net-policy: %w", err)
	}

	// Create-or-reset the per-VM chain, populate it, then (idempotently) jump
	// FORWARD to it. -F before -X-less reuse handles a restart re-applying.
	cmds := []string{
		fmt.Sprintf("sudo iptables -N %s 2>/dev/null || sudo iptables -F %s", chain, chain),
	}
	for _, l := range lines {
		cmds = append(cmds, "sudo iptables "+l)
	}
	cmds = append(cmds, fmt.Sprintf(
		"sudo iptables -C FORWARD -i %s -j %s 2>/dev/null || sudo iptables -I FORWARD 1 -i %s -j %s",
		vm.TAPDevice, chain, vm.TAPDevice, chain))

	if _, err := ex.Run(strings.Join(cmds, " && ")); err != nil {
		return fmt.Errorf("apply host net-policy: %w", err)
	}
	return nil
}

// RemoveNetworkPolicyHost tears down the per-VM chain and its FORWARD jump.
// Best-effort: errors are swallowed (mirrors RemovePortForwarding).
func RemoveNetworkPolicyHost(ex Executor, vm *state.VM) {
	if vm.TAPDevice == "" {
		return
	}
	chain := hostPolicyChain(vm.TAPDevice)
	ex.Run(fmt.Sprintf(
		"sudo iptables -D FORWARD -i %s -j %s 2>/dev/null; "+
			"sudo iptables -F %s 2>/dev/null; "+
			"sudo iptables -X %s 2>/dev/null",
		vm.TAPDevice, chain, chain, chain))
}
```

- [ ] **Step 4: Wire apply into the daemon post-boot goroutine**

In `internal/server/routes.go`, in `handleCreateVM`'s post-boot goroutine, add the host-side call immediately *before* the existing guest-side call (host-side is authoritative; guest-side stays as defense-in-depth). Change the block at `routes.go:259-261` from:

```go
		if err := firecracker.ApplyNetworkPolicyViaAgent(s.executor, postVM); err != nil {
			log.Printf("VM %s: network policy setup failed: %v", req.Name, err)
		}
```

to:

```go
		// Host-side enforcement (authoritative — the guest cannot flush FORWARD
		// rules in the Lima VM). The guest-side call below stays as
		// defense-in-depth.
		if err := firecracker.ApplyNetworkPolicyHost(s.executor, postVM); err != nil {
			log.Printf("VM %s: host network policy setup failed: %v", req.Name, err)
		}
		if err := firecracker.ApplyNetworkPolicyViaAgent(s.executor, postVM); err != nil {
			log.Printf("VM %s: guest network policy (defense-in-depth) setup failed: %v", req.Name, err)
		}
```

- [ ] **Step 5: Wire remove into stop and delete**

In `internal/server/routes.go`, in `handleStopVM`, next to the existing `firecracker.RemovePortForwarding(s.executor, vm)` (`routes.go:495`), add on the following line:

```go
	firecracker.RemoveNetworkPolicyHost(s.executor, vm)
```

And in `internal/firecracker/process.go`'s `Cleanup` (`process.go:149`), which runs on delete (`routes.go:467`), call `RemoveNetworkPolicyHost(ex, vm)` at the top of the function body so a deleted VM never leaves an orphaned chain. (The tap device itself is torn down later in the same cleanup script; removing the jump first avoids a dangling reference.)

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/firecracker/ -run 'HostPolicy|NetworkPolicyHost' -v && go build ./...`
Expected: PASS and clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/firecracker/netpolicy.go internal/firecracker/netpolicy_test.go internal/server/routes.go internal/firecracker/process.go
git commit -m "feat(firecracker): host-side net-policy via FORWARD chain in Lima"
```

---

### Task 3: applevz privilege installer — `mvm net-setup`

**Files:**
- Create: `internal/cli/netsetup.go`
- Test: `internal/cli/netsetup_test.go`
- Modify: `internal/cli/root.go` (register the command)

**Interfaces:**
- Produces:
  - `func newNetSetupCmd() *cobra.Command` — the `mvm net-setup` command.
  - `func pfSudoersContent(user string) string` — the exact `/etc/sudoers.d/mvm-pf` body (pure, unit-tested).
  - `const pfConfAnchorLine = "anchor \"mvm/*\""` and `func pfConfNeedsAnchor(existing string) bool` — pure idempotency check for editing `/etc/pf.conf`.
- Consumed by: the operator once (documented setup); Task 5's apply path assumes it has been run.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/netsetup_test.go`:

```go
package cli

import (
	"strings"
	"testing"
)

func TestPFSudoersContentScopesToMvmAnchor(t *testing.T) {
	body := pfSudoersContent("alice")
	if !strings.Contains(body, "alice ALL=(root) NOPASSWD:") {
		t.Errorf("sudoers must grant NOPASSWD to the invoking user:\n%s", body)
	}
	if !strings.Contains(body, "/sbin/pfctl -a mvm/*") {
		t.Errorf("sudoers must scope pfctl to the mvm/* anchor:\n%s", body)
	}
	// Must NOT hand out a blanket pfctl.
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "/sbin/pfctl") || strings.Contains(line, "pfctl *") {
			t.Errorf("sudoers must not grant unrestricted pfctl: %q", line)
		}
	}
}

func TestPFConfNeedsAnchor(t *testing.T) {
	if !pfConfNeedsAnchor("scrub-anchor \"com.apple/*\"\n") {
		t.Error("a pf.conf without the mvm anchor needs it added")
	}
	if pfConfNeedsAnchor("nat-anchor \"com.apple/*\"\nanchor \"mvm/*\"\n") {
		t.Error("a pf.conf already carrying the mvm anchor must not be edited again")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'PFSudoers|PFConfNeedsAnchor' -v`
Expected: FAIL — `undefined: pfSudoersContent`, `undefined: pfConfNeedsAnchor`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/netsetup.go`:

```go
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// pfConfAnchorLine is the filter anchor mvm references from the main pf ruleset.
// mvm populates per-VM sub-anchors (mvm/<name>) under it at VM-start time.
const pfConfAnchorLine = `anchor "mvm/*"`

const pfSudoersPath = "/etc/sudoers.d/mvm-pf"
const pfConfPath = "/etc/pf.conf"

// pfSudoersContent is the /etc/sudoers.d/mvm-pf body. It grants the invoking
// user passwordless pfctl for ONLY the mvm/* anchor operations mvm performs at
// runtime — never a blanket pfctl. VM names are validated to [a-zA-Z0-9._-]
// (state.ValidateName) so the mvm/* wildcard cannot be widened via a crafted
// name.
func pfSudoersContent(user string) string {
	return fmt.Sprintf(`# Managed by `+"`mvm net-setup`"+` — passwordless pf anchor control for host-side --net-policy.
%s ALL=(root) NOPASSWD: /sbin/pfctl -a mvm/* -f -
%s ALL=(root) NOPASSWD: /sbin/pfctl -a mvm/* -F all
`, user, user)
}

// pfConfNeedsAnchor reports whether /etc/pf.conf still needs the mvm anchor
// appended (idempotent — running net-setup twice must not duplicate the line).
func pfConfNeedsAnchor(existing string) bool {
	return !strings.Contains(existing, pfConfAnchorLine)
}

func newNetSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "net-setup",
		Short: "One-time host setup for applevz host-side --net-policy (installs a pf anchor; requires sudo)",
		Long: `Installs the privilege mechanism mvm uses to enforce --net-policy on the
host for Apple VZ VMs:

  1. writes ` + pfSudoersPath + ` granting your user passwordless pfctl for
     ONLY the mvm/* pf anchor, and
  2. adds a filter anchor "mvm/*" to ` + pfConfPath + ` and enables pf.

Run once, with sudo:  sudo mvm net-setup`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNetSetup()
		},
	}
}

func runNetSetup() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("mvm net-setup is only needed on macOS (Apple VZ); the Firecracker/Lima path uses the in-Lima executor")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("mvm net-setup must run as root: sudo mvm net-setup")
	}

	// The user we grant passwordless pfctl to is the one who invoked sudo.
	target := os.Getenv("SUDO_USER")
	if target == "" {
		if u, err := user.Current(); err == nil {
			target = u.Username
		}
	}
	if target == "" || target == "root" {
		return fmt.Errorf("could not determine the non-root user to grant pf access; run via `sudo mvm net-setup` from your normal account")
	}

	// 1. sudoers drop-in (0440, root:wheel — sudo refuses looser modes).
	if err := os.WriteFile(pfSudoersPath, []byte(pfSudoersContent(target)), 0o440); err != nil {
		return fmt.Errorf("write %s: %w", pfSudoersPath, err)
	}
	// Validate the drop-in before trusting it; remove it if visudo rejects it.
	if out, err := exec.Command("visudo", "-cf", pfSudoersPath).CombinedOutput(); err != nil {
		os.Remove(pfSudoersPath)
		return fmt.Errorf("sudoers validation failed, drop-in removed: %s", strings.TrimSpace(string(out)))
	}

	// 2. pf.conf anchor (idempotent).
	conf, err := os.ReadFile(pfConfPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", pfConfPath, err)
	}
	if pfConfNeedsAnchor(string(conf)) {
		updated := string(conf)
		if !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		updated += pfConfAnchorLine + "\n"
		if err := os.WriteFile(pfConfPath, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("update %s: %w", pfConfPath, err)
		}
	}

	// 3. Load the updated ruleset and enable pf (idempotent; -e is a no-op /
	// harmless "already enabled" if pf is already on).
	if out, err := exec.Command("pfctl", "-f", pfConfPath).CombinedOutput(); err != nil {
		return fmt.Errorf("pfctl -f %s: %s", pfConfPath, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("pfctl", "-e").Run() // returns nonzero if already enabled

	fmt.Printf("  ✓ mvm host-side net-policy is set up for user %q.\n", target)
	fmt.Println("    Apple VZ VMs started with --net-policy deny/allow are now enforced on the host.")
	return nil
}
```

In `internal/cli/root.go`, add `newNetSetupCmd(),` to the `rootCmd.AddCommand(...)` list (it takes no `store`, matching e.g. other stateless subcommands).

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/cli/ -run 'PFSudoers|PFConfNeedsAnchor' -v && go build ./...`
Expected: PASS and clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/netsetup.go internal/cli/netsetup_test.go internal/cli/root.go
git commit -m "feat(cli): mvm net-setup — one-time pf privilege installer for applevz net-policy"
```

---

### Task 4: applevz host-side apply + remove (via sudo pfctl)

**Files:**
- Create: `internal/vm/pfpolicy.go`
- Test: `internal/vm/pfpolicy_test.go`

**Interfaces:**
- Consumes: `netpolicy.Resolve`, `netpolicy.PFAnchorRules` (Task 1).
- Produces:
  - `func pfAnchorName(vmName string) string` — `"mvm/"+vmName`.
  - `func pfApplyCommand(vmName string) []string` — the exact `pfctl` argv (`sudo pfctl -a mvm/<name> -f -`) as a slice, pure and unit-tested (the rules go in on stdin).
  - `func pfFlushCommand(vmName string) []string` — `sudo pfctl -a mvm/<name> -F all`.
  - `func ApplyNetworkPolicyPF(vmName, guestIP, netPolicy string) error` — resolves, generates, and pipes the anchor body to `sudo pfctl`. No-op for open.
  - `func RemoveNetworkPolicyPF(vmName string)` — best-effort anchor flush.

- [ ] **Step 1: Write the failing test**

Create `internal/vm/pfpolicy_test.go`:

```go
package vm

import (
	"strings"
	"testing"
)

func TestPFAnchorName(t *testing.T) {
	if got := pfAnchorName("web"); got != "mvm/web" {
		t.Errorf("pfAnchorName = %q, want mvm/web", got)
	}
}

func TestPFApplyCommandShape(t *testing.T) {
	argv := pfApplyCommand("web")
	got := strings.Join(argv, " ")
	if got != "sudo pfctl -a mvm/web -f -" {
		t.Errorf("pfApplyCommand = %q, want `sudo pfctl -a mvm/web -f -`", got)
	}
}

func TestPFFlushCommandShape(t *testing.T) {
	argv := pfFlushCommand("web")
	got := strings.Join(argv, " ")
	if got != "sudo pfctl -a mvm/web -F all" {
		t.Errorf("pfFlushCommand = %q, want `sudo pfctl -a mvm/web -F all`", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/vm/ -run 'PFAnchor|PFApply|PFFlush' -v`
Expected: FAIL — `undefined: pfAnchorName`, `undefined: pfApplyCommand`, `undefined: pfFlushCommand`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/vm/pfpolicy.go`:

```go
package vm

import (
	"bytes"
	"fmt"
	"net"
	"os/exec"

	"github.com/agentstep/mvm/internal/netpolicy"
)

// pfAnchorName is the per-VM pf sub-anchor mvm populates. The parent anchor
// "mvm/*" is referenced from /etc/pf.conf by `mvm net-setup`.
func pfAnchorName(vmName string) string { return "mvm/" + vmName }

// pfApplyCommand is the exact argv that loads a VM's anchor body from stdin.
// Matches the /etc/sudoers.d/mvm-pf grant (`/sbin/pfctl -a mvm/* -f -`) so it
// runs passwordless.
func pfApplyCommand(vmName string) []string {
	return []string{"sudo", "pfctl", "-a", pfAnchorName(vmName), "-f", "-"}
}

// pfFlushCommand is the exact argv that clears a VM's anchor. Matches the
// sudoers grant `/sbin/pfctl -a mvm/* -F all`.
func pfFlushCommand(vmName string) []string {
	return []string{"sudo", "pfctl", "-a", pfAnchorName(vmName), "-F", "all"}
}

// ApplyNetworkPolicyPF enforces netPolicy for an applevz VM on the HOST via a
// per-VM pf anchor keyed on its DHCP-assigned guestIP — the guest cannot touch
// host pf. Requires `mvm net-setup` to have been run (installs the sudoers
// grant + pf.conf anchor). No-op for open. Domains resolved host-side at apply
// time (see the plan's DNS design decision).
func ApplyNetworkPolicyPF(vmName, guestIP, netPolicy string) error {
	if netPolicy == "" || netPolicy == "open" {
		return nil
	}
	if guestIP == "" {
		return fmt.Errorf("apply pf net-policy: VM %q has no guest IP yet", vmName)
	}
	allowedIPs, err := netpolicy.Resolve(netPolicy, net.LookupHost)
	if err != nil {
		return fmt.Errorf("apply pf net-policy: %w", err)
	}
	body, err := netpolicy.PFAnchorRules(guestIP, netPolicy, allowedIPs)
	if err != nil {
		return fmt.Errorf("apply pf net-policy: %w", err)
	}

	argv := pfApplyCommand(vmName)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewBufferString(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply pf net-policy: pfctl: %s: %w — did you run `sudo mvm net-setup`?", string(out), err)
	}
	return nil
}

// RemoveNetworkPolicyPF clears a VM's pf anchor. Best-effort (mirrors
// RemovePortForwarding / RemoveNetworkPolicyHost).
func RemoveNetworkPolicyPF(vmName string) {
	argv := pfFlushCommand(vmName)
	_ = exec.Command(argv[0], argv[1:]...).Run()
}
```

- [ ] **Step 4: Wire apply into runStartAppleVZ**

In `internal/cli/start.go`, host-side pf apply must happen once `guestIP` is known (after the `NetInfo` discovery block that sets `updatedVM.GuestIP` at `start.go:387-402`) and should run *before* the existing guest-side `applyVZNetworkPolicy` call (`start.go:405`). Replace the block at `start.go:404-407`:

```go
		// Apply network policy via the agent.
		if err := applyVZNetworkPolicy(ctx, agent, netPolicy); err != nil {
			logf("  Warning: apply network policy: %v\n", err)
		}
```

with:

```go
		// Host-side enforcement (authoritative — a pf anchor keyed on the
		// guest's DHCP IP, which the guest cannot reach). Requires a one-time
		// `sudo mvm net-setup`. The guest-side call below stays as
		// defense-in-depth.
		if updatedVM.GuestIP != "" {
			if err := vm.ApplyNetworkPolicyPF(name, updatedVM.GuestIP, netPolicy); err != nil {
				logf("  Warning: apply host net-policy (pf): %v\n", err)
			}
		}
		// Guest-side enforcement (defense-in-depth).
		if err := applyVZNetworkPolicy(ctx, agent, netPolicy); err != nil {
			logf("  Warning: apply guest network policy (defense-in-depth): %v\n", err)
		}
```

(`vm` is already imported in `start.go:17` as `"github.com/agentstep/mvm/internal/vm"`.)

- [ ] **Step 5: Wire remove into applevz teardown**

In `internal/cli/delete.go`, in `runDeleteAppleVZ`, after the `killForwarder` call (`delete.go:130`) and before removing the state dir, add:

```go
	// Drop the host-side pf anchor (no-op if net-setup was never run or the
	// policy was open). Best-effort — never blocks deletion.
	vm_pkg.RemoveNetworkPolicyPF(name)
```

(The file already imports the vm package aliased as `vm_pkg` — see `delete.go:119`'s `vm_pkg.NewAppleVZBackend`.)

Also in `internal/cli/stop.go`, wherever the applevz stop path tears a VM down (the `runStopAppleVZ` equivalent that calls `vzBackend.StopVM`), add the same `vm_pkg.RemoveNetworkPolicyPF(name)` best-effort call so a stopped VM's anchor doesn't linger. If `stop.go` has no applevz-specific teardown function, add the call immediately after the applevz `StopVM` invocation. Grep to confirm the exact site:

```bash
grep -n "StopVM\|applevz\|AppleVZ" internal/cli/stop.go
```

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/vm/ -run 'PFAnchor|PFApply|PFFlush' -v && go build ./...`
Expected: PASS and clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/vm/pfpolicy.go internal/vm/pfpolicy_test.go internal/cli/start.go internal/cli/delete.go internal/cli/stop.go
git commit -m "feat(applevz): host-side net-policy via per-VM pf anchor"
```

---

### Task 5: Re-label guest-side enforcement as defense-in-depth (rollout decision)

**Files:**
- Modify: `internal/firecracker/process.go` (doc comment on `ApplyNetworkPolicyViaAgent`)
- Modify: `internal/cli/start.go` (doc comment on `applyVZNetworkPolicy`)

**Interfaces:** none — comment-only; no behavior change. This task records the rollout/removal decision in the code itself.

**Decision (justified):** KEEP guest-side enforcement running as belt-and-braces during and after this change. The host-side layer is now authoritative and is what the security posture relies on; the guest-side layer is redundant but harmless, and removing it in the same change would mean that any host-side gap discovered later has no fallback at all. Removal of the guest-side layer is deferred to a separate, independently-verified change once host-side enforcement has been confirmed in production on both backends.

- [ ] **Step 1: Update the Firecracker comment**

In `internal/firecracker/process.go`, replace the `ApplyNetworkPolicyViaAgent` doc comment (`process.go:165`) with:

```go
// ApplyNetworkPolicyViaAgent sets iptables OUTPUT rules INSIDE the guest via
// the agent. As of the host-side net-policy change this is DEFENSE-IN-DEPTH
// only: the authoritative block is ApplyNetworkPolicyHost (FORWARD-chain rules
// in the Lima VM, which the guest cannot flush). A root process in the guest
// CAN flush these guest-side rules — that is exactly why the host-side layer
// exists — so a failure here is logged, not fatal.
```

- [ ] **Step 2: Update the applevz comment**

In `internal/cli/start.go`, replace the `applyVZNetworkPolicy` doc comment (`start.go:505-508`) with:

```go
// applyVZNetworkPolicy enforces a network policy by issuing iptables rules
// INSIDE the guest via the agent. As of the host-side net-policy change this is
// DEFENSE-IN-DEPTH only: the authoritative block is vm.ApplyNetworkPolicyPF (a
// host pf anchor keyed on the guest's DHCP IP, which the guest cannot reach).
```

- [ ] **Step 3: Build to confirm comments compile**

Run: `go build ./...`
Expected: clean build (comment-only change).

- [ ] **Step 4: Commit**

```bash
git add internal/firecracker/process.go internal/cli/start.go
git commit -m "docs: mark guest-side net-policy as defense-in-depth (host-side is authoritative)"
```

---

### Task 6: Verification — unit suite + documented live-backend manual checks

**Files:** none (verification only).

**Interfaces:** none — runs the suite and documents manual steps.

- [ ] **Step 1: Full module build, vet, and unit test**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: clean build, `go vet` silent, every package `ok` (hardware/live-backend-dependent tests may skip; no FAILs). The new pure packages `internal/netpolicy`, plus the new tests in `internal/firecracker`, `internal/cli`, and `internal/vm`, all pass.

- [ ] **Step 2: Confirm no existing behavior regressed**

Run: `go test ./internal/firecracker/ ./internal/cli/ ./internal/vm/ ./internal/server/ -v 2>&1 | tail -40`
Expected: PASS — pre-existing tests (port forwarding, security, start/exec/delete) are untouched and still pass.

- [ ] **Step 3: Manual live verification — Firecracker/Lima `deny` (requires an initialized Firecracker host)**

These cannot be unit-tested (they need a live Lima VM + guest). Run on a Firecracker-initialized machine:

```bash
go run ./cmd/mvm start denybox --net-policy deny
# Outbound must FAIL (host-side FORWARD DROP), DNS must still resolve:
go run ./cmd/mvm exec denybox -- sh -c 'getent hosts github.com && echo DNS_OK'
go run ./cmd/mvm exec denybox -- sh -c 'curl -sS --max-time 5 https://github.com; echo "exit=$?"'
# Expected: DNS_OK printed; curl fails (non-zero exit / timeout).

# Prove host-side is authoritative: flush the GUEST rules, curl must STILL fail.
go run ./cmd/mvm exec denybox -- iptables -F OUTPUT
go run ./cmd/mvm exec denybox -- sh -c 'curl -sS --max-time 5 https://github.com; echo "exit=$?"'
# Expected: curl STILL fails — the FORWARD-chain block in Lima is untouched by
# guest-side iptables -F. This is the whole point of the change.

# Confirm the chain exists in Lima (tap name from `mvm inspect`):
limactl shell mvm sudo iptables -S FORWARD | grep MVM-
go run ./cmd/mvm delete denybox --force
# After delete, the chain is gone:
limactl shell mvm sudo iptables -S | grep MVM- || echo "chain cleaned up"
```

- [ ] **Step 4: Manual live verification — Firecracker/Lima `allow:` **

```bash
go run ./cmd/mvm start allowbox --net-policy allow:github.com
go run ./cmd/mvm exec allowbox -- sh -c 'curl -sS --max-time 8 -o /dev/null -w "%{http_code}\n" https://github.com'   # expect 200/301
go run ./cmd/mvm exec allowbox -- sh -c 'curl -sS --max-time 5 https://example.com; echo "exit=$?"'                    # expect failure
go run ./cmd/mvm delete allowbox --force
```

- [ ] **Step 5: Manual live verification — applevz `deny` (requires an Apple-silicon Mac + `sudo mvm net-setup`)**

```bash
sudo go run ./cmd/mvm net-setup          # one-time; installs sudoers + pf.conf anchor
go run ./cmd/mvm start vzdeny --net-policy deny
# Inspect the pf anchor the host installed (keyed on the guest's DHCP IP):
sudo pfctl -a mvm/vzdeny -sr              # expect the block + DNS-pass rules
go run ./cmd/mvm exec vzdeny -- sh -c 'curl -sS --max-time 5 https://github.com; echo "exit=$?"'   # expect failure
go run ./cmd/mvm exec vzdeny -- sh -c 'getent hosts github.com && echo DNS_OK'                      # expect DNS_OK
# Authoritative check: flush guest rules, curl STILL fails (host pf untouched):
go run ./cmd/mvm exec vzdeny -- iptables -F OUTPUT
go run ./cmd/mvm exec vzdeny -- sh -c 'curl -sS --max-time 5 https://github.com; echo "exit=$?"'   # expect failure
go run ./cmd/mvm delete vzdeny --force
sudo pfctl -a mvm/vzdeny -sr || echo "anchor cleaned up"   # expect empty/cleaned
```

- [ ] **Step 6: Commit (only if Steps 1-2 surfaced a fix)**

If Steps 1-2 passed clean there is nothing to commit. Otherwise:

```bash
git add -A
git commit -m "fix: address host-side net-policy verification findings"
```

---

## Out of Scope (explicitly)

- **A DNS-intercepting proxy for robust allow-lists.** v1 is resolve-at-apply-time (documented trade-off in §Design decisions). Making allow-lists survive DNS re-resolution needs a live host resolver pinning A-records into a pf table/ipset — a separate, larger workstream.
- **Removing the guest-side enforcement.** Kept as defense-in-depth (Task 5 decision); removal is a later, independently-verified change.
- **IPv6 allow-lists.** `Resolve` filters to IPv4 for v1 (the guest networking is IPv4/DHCP today). IPv6 pf/ip6tables rules are a follow-up.
- **applevz `allow:` with per-VM live IP refresh.** Same staleness limitation as Firecracker; both resolve at apply time.
- **`--image` on applevz** and any CLI-surface change — untouched (constraint).
- **Automatically running `net-setup` from `mvm init`.** Kept as an explicit, separately-invoked `sudo mvm net-setup` so the privileged step is visible and auditable, not buried in init. Folding it into an interactive `mvm init` prompt is a possible later UX improvement, not part of this plan.
</content>
</invoke>
