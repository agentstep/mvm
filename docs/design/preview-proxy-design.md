# Design: authenticated preview-port proxy (workdir-parity Phase 4)

Status: proposed (decision doc). Supersedes the one-paragraph Phase 4 sketch in
`workdir-parity-plan.md`. Written after an architect review found the original
design infeasible for the Apple VZ backend.

## Problem

Agents want to expose a service running inside a sandbox — a dev server, a
notebook, a browser-automation VNC — at a URL they can open or share. workdir
and Fly do this; mvm doesn't. This doc settles the architecture and the
security model before any code, because a proxy that forwards untrusted guest
traffic to a browser is the highest-risk surface in the product.

## The load-bearing constraint (why the obvious design fails)

The naive design — "host reverse proxy → `guestIP:port`" — **cannot work on the
Apple VZ backend**, which is our VZ-first target:

- The applevz guest IP (`172.16.0.x`, set via the kernel `ip=` boot arg in
  `internal/cli/start.go`) lives behind Apple's `VZNATNetworkDeviceAttachment`
  (`vz/Sources/mvm-vz/VM/VMManager.swift`). That NAT is **internal to the helper
  process**; the guest IP is **not host-routable**. There is no TAP device
  (`start.go` sets `v.TAPDevice = ""`), and nothing on the host answers on that
  subnet.
- The **only** host→guest path is the vsock fd handed back over the per-VM
  helper IPC socket (`internal/vzhelper/client.go` `Connect`, via `SCM_RIGHTS`),
  and it currently reaches exactly **one** guest service: the mvm-agent on vsock
  port 5123. The agent protocol (`agent/main.go`) has `ping/exec/exec_pty/
  read_file/write_file/setup_net/poweroff` — **no TCP-forward verb.**

**Therefore the guest-reachability primitive must be built first.** Everything
else is a proxy on top of it.

## Architecture

```
                         ┌─────────────────────────── host (macOS) ──────────┐
browser ──HTTPS──> mvm preview listener ──vsock(SCM_RIGHTS)──> mvm-agent ──┐  │
   (cookie auth)   (internal/preview)                          (tcp_forward)│  │
                          │                                                 ▼  │
                          └── GuestDialer (VZ: vsock / FC: net.Dial) ──> 127.0.0.1:port (guest)
                                                                            (inside the VM)
```

### 1. New guest primitive: agent `tcp_forward` verb

Add a `tcp_forward` request to the agent (`agent/main.go` + protocol): given a
`guest_port`, the agent dials `127.0.0.1:<port>` **inside the guest**, replies
OK, then the connection becomes a transparent bidirectional `io.Copy` relay —
exactly the pattern the existing PTY hijack relay already uses
(`internal/server/routes.go`). The host opens a fresh vsock fd (helper
`Connect` → port 5123), sends the request, and relays bytes between the inbound
proxy connection and the vsock conn.

Because the dial target is chosen **inside the guest** (always loopback), the
host never picks an IP — so host-side SSRF to host loopback is *structurally
impossible* on VZ. This is the key safety property of routing through the agent.

### 2. Backend-agnostic proxy: `internal/preview`

A `GuestDialer` interface decouples the proxy from the backend:

```go
type GuestDialer interface {
    Dial(vm *state.VM, guestPort int) (net.Conn, error)
}
```

- **VZ dialer** (`dial_vz.go`): helper `Connect` → `tcp_forward` → relay conn.
- **FC dialer** (`dial_fc.go`): validate target == `vm.GuestIP`, then `net.Dial`
  tcp (runs inside Lima, where the guest IP *is* routable).

The HTTP handler (port-allowlist, auth, header scrubbing, websocket upgrade,
timeouts) is shared; only the dialer differs per backend. **VZ-first:** ship the
agent verb + VZ dialer + the listener first; the FC daemon route follows.

### 3. Where the listener runs

- **applevz:** a dedicated **mac-side** listener — `mvm preview` — because the
  helper sockets live on macOS, not in Lima. It reads `state.json`, resolves
  `vm`→helper socket, validates `Ports`, carries its own API key + TLS. It is
  **not** the Firecracker daemon (`internal/server`), which runs in Lima and
  cannot route to VZ guests. Do not force applevz through `mvm serve`.
- **firecracker:** register the route on the in-Lima daemon's **authed TCP
  listener only** (never the unauthenticated Unix socket).

## Decisions

### D1 — Browser auth carrier → **signed cookie via one-time token exchange**

Browsers don't send `Authorization: Bearer`. Options:
- (a) **Signed cookie**, obtained by hitting `…/p/<vm>/<port>/__mvm_auth?token=<t>`
  once; the proxy validates `t` (an opaque, per-VM, expiring token minted by
  `mvm preview token <vm>`), sets an `HttpOnly; Secure; SameSite=Lax`,
  **path-scoped** cookie, and redirects. **Recommended.**
- (b) Trust an external front-proxy (Cloudflare Access / oauth2-proxy). Defer —
  it's an ops choice we can document later, not a default.

The cookie/token is **never** the raw control-plane API key, and is **stripped
before forwarding upstream** (the guest must never see it). Use
`crypto/subtle.ConstantTimeCompare` for all token/key checks (the current
`auth.go` compare is not constant-time — fix as part of this work).

### D2 — Per-VM origin isolation → **per-VM subdomain (`<vm>.preview.<host>`)**

Path-based (`/p/<vm>/`) shares **one origin across all VMs**. Serving untrusted
guest HTML + active content from a shared origin is a cross-VM XSS / cookie-theft
hole: a malicious guest's page can script same-origin requests to another VM's
path and read its path-scoped data. Per-VM subdomains give each sandbox its own
browser origin, so the same-origin policy does the isolation for us.

**Recommended: per-VM subdomain**, requiring wildcard DNS (`*.preview.<host>`) +
a wildcard TLS cert. Cost: the operator must provision those. For the
**localhost/dev default** (D3) where there's no public DNS, fall back to
`127.0.0.1:<assigned-port>` per VM — a distinct origin per port, same isolation
property, no wildcard needed. Path-based is **rejected** for any multi-VM,
cookie-authed, public deployment.

### D3 — Network exposure → **localhost-only by default; public is explicit opt-in**

Default `mvm preview` binds `127.0.0.1` and assigns a local port per (vm,port) —
a `kubectl port-forward`-style local tunnel, no auth headaches, no public attack
surface. Going public (`--listen 0.0.0.0` / a real hostname) is an explicit flag
that **requires** TLS + a token and turns on the full D1/D2 machinery. We never
bind a proxy-to-untrusted-guest to `0.0.0.0` implicitly.

### D4 — Agent `tcp_forward` scope → **loopback-only inside the guest**

The agent dials only `127.0.0.1:<port>` in the guest. It does **not** accept a
host/IP from the caller and does **not** forward to the guest's LAN. This kills a
class of guest-side SSRF (a compromised control path can't pivot through the
agent to other hosts) and keeps the verb dead-simple.

### D5 — `Ports` semantics → **declared-at-create, deny-by-default**

The proxy forwards only ports present in `vm.Ports` (declared via `-p` at
`mvm start`). A port not declared is a uniform 404. This is deny-by-default and
needs no guest cooperation. UX cost: you must `mvm start --publish 3000:3000` to
preview `:3000` later; acceptable and safe. (A future `mvm preview <vm> <port>`
could add the port to `Ports` on the fly behind an explicit confirmation.)

### D6 — Firecracker parity → **later**

VZ-first: agent verb + `internal/preview` + `mvm preview` ship first. The FC
daemon route (simpler dial, but different Lima host-exposure story) follows once
the VZ path is proven.

## Security model (must-haves, all enforced)

1. **Uniform 404, authorize-before-existence.** Nonexistent VM, not-running,
   undeclared port, or unauthorized caller → byte-identical 404, no VM name
   echoed, no timing tell. (Today's handlers leak this — `routes.go` returns
   `"VM %q is %s"`; the preview path must not.)
2. **Port allowlist** — only `vm.Ports`; hard-deny **5123** (agent) and, on FC,
   the daemon port **19876** and its Unix socket.
3. **FC dial pinning** — dial *exactly* `vm.GuestIP`; reject `127.0.0.0/8`,
   `169.254.0.0/16` (incl. metadata `169.254.169.254`), `::1`, the Lima host IP.
   Never dial a hostname or anything attacker-influenced. (VZ is immune by
   construction — the guest dials its own loopback.)
4. **Strip credentials upstream** — remove `Authorization`, the preview `Cookie`,
   and any `?token=` before bytes reach the guest. Strip hop-by-hop headers;
   handle `Upgrade`/websockets explicitly.
5. **Scrub guest response headers** — constrain/strip `Set-Cookie` to the
   proxy's own path; add `X-Content-Type-Options: nosniff` and a restrictive
   `Content-Security-Policy`. Combined with D2 (per-VM origin) this contains a
   malicious guest page.
6. **No shell** — the relay is pure Go `io.Copy`; the URL `port` is parsed to int
   and matched against `vm.Ports`, never interpolated into a command (unlike
   `firecracker/network.go`'s iptables strings).
7. **Resource bounds** — per-conn idle/read/write timeouts; cap concurrent
   forwards per VM; both relay goroutines terminate when either side closes
   (copy the teardown in `routes.go`).

## Implementation phases

- **4a — primitive + local tunnel (safe, ship first).** Agent `tcp_forward`
  verb; `internal/preview` handler + VZ dialer; `mvm preview <vm> <port>` binding
  `127.0.0.1` (D3 default). Guards 1–7 minus the browser-auth/cookie parts.
  Tests: port-not-declared → 404, identical 404 for missing VM, 5123 denied,
  bidirectional relay, idle timeout, forward-to-closed-guest-port errors cleanly.
- **4b — public proxy (gated on operator config).** Per-VM subdomain origins
  (D2), one-time-token→signed-cookie auth (D1), `--listen`/TLS, header scrubbing
  + CSP (guards 4–5 full), constant-time auth fix. Tests: token exchange, cookie
  scoping, credential-strip-before-upstream, websocket upgrade, cross-VM origin
  isolation.
- **4c — Firecracker parity (D6).** FC dialer with dial-pinning; daemon route on
  the authed TCP listener; FC dial-target tests.

## Out of scope / accepted limitations

- Subpath rewriting for dev servers that assume they own `/` (absolute redirects
  may break) — documented limitation, same as workdir.
- Public exposure without a user-provided tunnel/TLS is not a supported default.
- `Ports` reflects *declared*, not *currently-listening* — a declared port with
  nothing behind it yields a clean connection error, not a hang.

## Open questions for the human

1. **DNS/TLS for per-VM subdomains (D2).** Are we willing to require wildcard
   DNS + wildcard TLS for the public path, or should public 4b wait until
   there's demand and stay localhost-only (4a) for now?
2. **Token minting UX (D1).** `mvm preview token <vm>` printing a URL — good
   enough, or do we want short-lived links with a TTL flag?
3. **Scope of first delivery.** Ship 4a only (local tunnel — genuinely useful,
   zero public risk) and defer 4b/4c until the DNS/TLS questions are answered?
