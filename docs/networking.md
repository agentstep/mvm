# Network policy

`--net-policy` controls what a sandbox can reach.

| Policy | Effect |
|---|---|
| `open` (default) | No filter. |
| `deny` | Every packet the guest originates is dropped, **including DNS**. |
| `allow:a.com,b.com` | Only addresses resolved for the listed domains and their subdomains are reachable. |

## Where enforcement happens

On the **Firecracker** backend, policy is enforced by a per-VM chain
(`MVM-EGRESS-<index>`) in the Lima VM, outside the sandbox. The chain is
installed before the microVM boots and torn down with its TAP device.

This is the point: the guest runs as root. Any rule installed *inside* it can be
flushed by the very process it is meant to contain. Rules on the host cannot.

`mvm exec` keeps working under every policy — vsock is not IP and never
traverses these chains. The operator keeps control; the guest loses the network.

Published ports (`-p 3000:3000`) keep working under `deny`. Only traffic
arriving on the VM's TAP device is filtered; DNAT'd inbound connections arrive
on another interface.

## `deny` drops DNS — a deliberate behaviour change

The previous in-guest ruleset carved out port 53. A resolver the guest can still
reach is an exfiltration channel, so under a policy named `deny` it is now
closed. Anything relying on name resolution under `deny` will stop working; use
`allow:` with an explicit domain list instead.

## `allow:` and DNS

Each allow-policy VM gets a DNS proxy bound to its own gateway address, and the
filter permits port 53 only to that address. A name off the allowlist is refused
without being forwarded upstream — forwarding it would leak the lookup itself.
Every address the proxy returns is added to the VM's ipset with the record's TTL
(floored at 60s), so CDN rotation is handled automatically.

An earlier implementation resolved each domain once with `getent` at boot and
pinned the resulting IPs. That broke open (a rotated-away address still allowed)
and closed (the current address blocked) unpredictably.

## IPv6

Policies are installed into `ip6tables` as well as `iptables`, with a separate
`inet6` ipset for allowlists — an ipset's address family is fixed at creation,
and adding an IPv6 address to an `inet` set fails rather than storing it.

This matters more than the current setup suggests. The guest has no IPv6 route
today, so v6 egress fails on its own — but guest DNS already returns AAAA
records, so `curl` without `-4` prefers a v6 path. A filter that holds only
because the transport happens to be unconfigured is not a filter, and one
routing change would silently open every policy.

## Apple VZ

**On the `applevz` backend `--net-policy` is still enforced inside the guest and
is a guardrail, not a boundary.** There is no host-side TAP device to filter on.
Use `--backend firecracker` where the policy needs to hold against the code
running in the sandbox.

This is demonstrable, not theoretical — verified on applevz, 2026-08-19:

    mvm start denytest --net-policy deny
    mvm exec denytest curl -4 -s -o /dev/null -w "%{http_code}" https://example.com
    # -> 000  (blocked)
    mvm exec denytest iptables -F OUTPUT
    mvm exec denytest curl -4 -s -o /dev/null -w "%{http_code}" https://example.com
    # -> 200  (the guest removed its own cage)

Force IPv4 when testing this. Without `-4`, DNS returns AAAA records and the
connection fails for lack of a v6 route regardless of policy, which reads as
"still blocked" and hides the escape.
