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
// silently allowing everything it was meant to block. Guest DNS already returns
// AAAA records today, so this is closer than it looks.
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
// process it is meant to contain. This reproduces today, on applevz: with
// --net-policy deny, `curl -4` returns 000; after `iptables -F OUTPUT` from
// inside the guest, it returns 200.
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
			// Replies to connections opened FROM outside are accepted; nothing the guest
			// originates is.
			//
			// Without this the chain was an unconditional DROP, and a published port was
			// silently dead: the inbound SYN reaches the guest (DNAT'd, so it never traverses
			// this chain) but the guest's SYN-ACK leaves on tapN and was dropped. Meanwhile
			// allow: has always carried this same accept, so the two policies disagreed about
			// whether ingress works — an accident in both directions, not a decision.
			//
			// This cannot become an egress hole. A guest-originated flow is dropped at its SYN,
			// so conntrack never promotes it past NEW — ESTABLISHED can only match a flow whose
			// first packet came from outside, i.e. a port the operator deliberately published.
			// "deny" now means exactly one thing: the guest may not originate connections.
			//
			// Still no DNS carve-out: a reachable resolver is an exfiltration channel under a
			// policy named "deny".
			b.WriteString("sudo \"$IPT\" -A \"$CHAIN\" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n")
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
