# mvm Product Plan — Learnings → Roadmap

> Companion to [`agent-sandbox-landscape.md`](./agent-sandbox-landscape.md).
> This turns the competitive research into a concrete, file-level engineering
> plan. Sequenced by leverage; effort is rough (S ≤ few days, M ≤ ~1–2 wks,
> L > 2 wks). Each item cites what already exists so we build on primitives
> rather than greenfield.

## Strategic frame

The research found mvm's wedge is real and largely unoccupied — **the only
Firecracker+KVM product with the same API local + self-hosted**. But several
competitors have *productized* primitives mvm only exposes as plumbing:

- **Sprites / Blaxel** lead with *persistent, idle-to-zero, instant-resume*
  sandboxes. mvm has `pause`/`resume`, snapshots, and UFFD lazy restore — but
  ships them as separate verbs, and `pause` still pins RAM.
- **Cloudflare / microsandbox** lead with *secrets never enter the VM* via a
  host-side egress proxy. mvm's `--net-policy` is enforced by **in-guest
  iptables** (`process.go:166`), which an adversarial root agent can tamper with.
- **Anthropic Managed Agents** ships a *self-hosted sandbox* backend protocol —
  with documented backends for Cloudflare/Daytona/Modal/Vercel but **no
  local-first, hardware-isolated option**. That's mvm's exact buyer.
- **Zeroboot** does ~0.8ms CoW VM fork; mvm's pool `cp --sparse`-copies a 2GB
  mem file per refill (`pool.go:281`), serially — the source of the 1.7s TTI
  gap vs Daytona/E2B's sub-200ms.

**Principles:** stay open-source + self-hostable; deepen the
compliance/sustained-workload story (our defensible buyer, not the
latency-only buyer who's fine with gVisor); productize existing primitives
before building new ones. **Non-goals (unchanged):** hosted SaaS, per-second
billing, gVisor, Windows, Docker packaging.

---

## Phase 1 — Persistent "idle-to-zero" sandboxes  · effort M

**Learn from:** Sprites (idle compute to $0, persistent FS), Blaxel (<25ms
resume with memory state). This is the feature the market leads with.

**Today.** `internal/cli/idle.go` already auto-pauses idle VMs (launchd, every
30s, `MVM_IDLE_TIMEOUT`), tracks `LastActivity`, and `AutoResumeIfPaused`
transparently un-pauses on next exec. **But:** (a) `Pause` (`network.go:58`)
only freezes the FC process — it still holds 2GB RAM, so it's not idle-to-*zero*;
(b) idle-check runs only via macOS launchd, not in the cloud daemon.

**Build.**
1. Add a deeper idle tier — **suspend-to-disk**: on idle timeout, `SnapshotVM`
   (`snapshot.go:28`) then terminate the FC process, freeing all RAM. Record
   `Status="suspended"` + snapshot path in state.
2. Extend `AutoResumeIfPaused` → `AutoResumeIfDormant`: on next `exec`, if
   suspended, `RestoreVMSnapshot` (`snapshot.go:106`) via the existing UFFD
   path (<100ms target) — transparent to the caller. Keep cheap `paused` as
   tier-1 (instant, holds RAM) and `suspended` as tier-2 (frees RAM, ~100ms
   resume).
3. Move idle-check into the daemon (`internal/server`) as a ticker so it works
   in **cloud mode**, not just macOS launchd. The launchd path becomes a thin
   client of the same logic.
4. Persistent FS is already satisfied — snapshots include `rootfs.ext4`.

**Surface.** `mvm idle enable --timeout 5m --suspend-after 30m`;
`mvm list` shows `running | paused | suspended`. SDKs: unchanged (resume is
transparent).

**Risk.** UFFD restore correctness under load (already the autoresearch
correctness gate). Mitigate: fall back to `paused` if snapshot/restore fails;
never lose the rootfs.

**Done when.** A VM idle past threshold drops to ~0 host RAM and the next
`exec` transparently restores and returns correct output in <300ms; verified
in both local and cloud mode.

---

## Phase 2 — Host-side egress proxy + credential injection  · effort M

**Learn from:** Cloudflare "Outbound Workers" (inject creds at the network
layer; agent never sees the token), microsandbox ("secrets never enter the
VM"), Gondolin (adversarial-guest / trusted-host enforcement).

**Today.** `--net-policy deny|allow:domains` is real but enforced by
**iptables inside the guest** (`process.go:166`) — a root agent can flush the
rules. Egress otherwise flows through Lima/host NAT untouched.

**Build.**
1. New `internal/proxy` — a host-side HTTP/HTTPS forward proxy run by the
   daemon, one logical instance per VM (or shared with per-VM identity by
   source TAP IP).
2. Force guest egress through it: set `HTTP_PROXY`/`HTTPS_PROXY` in the guest
   (via agent) **and** add a host-side iptables REDIRECT on the VM's TAP so
   raw egress can't bypass the proxy — enforcement moves host-side, where the
   guest can't tamper.
3. Policy lives host-side: allow/deny domains (replacing the guest-iptables
   approach) and **credential injection** — per-domain secret refs
   (`github.com → Authorization: Bearer <ref>`) that the proxy adds on the way
   out. Secrets sit in the daemon, never in the guest's env or disk.
4. Reuse the AES-256-GCM helpers in `snapshot.go:400` for the secret store at
   rest.

**Surface.** `mvm.policy.yaml` (see Backlog: declarative policy) or flags:
`--egress allow:github.com,pypi.org --inject github.com=GH_TOKEN`. SDK: pass a
policy object at create.

**Risk.** HTTPS interception needs either a guest-trusted CA (MITM) or
domain-level CONNECT allow-listing without inspecting payloads. Default to
**CONNECT allow-listing** (no MITM) for the security pitch; offer opt-in MITM
+ injection for users who install the CA. TLS-pinned clients won't accept MITM
— document it.

**Done when.** A guest with `--egress deny` + one injected domain can reach
only that domain, with the credential never present inside the VM (verified by
dumping guest env/FS), and flushing guest iptables does not widen access.

---

## Phase 3 — CoW snapshot fork + parallel pool refill  · effort M  *(feeds autoresearch)*

**Learn from:** Zeroboot (~0.8ms CoW fork via `mmap(MAP_PRIVATE)`), and the
sub-200ms creation times of Daytona/E2B. Directly attacks mvm's weakest metric
(TTI 1.7s) and two named autoresearch leverage points (snapshot copy, serial
refill).

**Today.** `restoreSlotFromSnapshot` (`pool.go:281`) does
`cp --sparse=always` of the ~2GB pristine mem file **per refill**, under
`nice/ionice`, and `WarmPool` (`pool.go:59`) refills **serially** to avoid
saturating a 4-core box.

**Build.**
1. Eliminate the 2GB copy: on reflink-capable FS use `cp --reflink=auto`
   (already roadmapped); otherwise have the UFFD handler `mmap(MAP_PRIVATE)`
   the immutable pristine mem file so guest writes are copy-on-write in page
   cache — no upfront copy. (`internal/uffd/handler.go`.)
2. Parallelize refill across slots now that each refill is cheap (no IO
   storm). Keep `warmPoolMu` but fan out per-slot fills with a bounded
   worker count = min(PoolSize, NumCPU-1).
3. Expose CoW fork as a first-class op: `RestoreVMSnapshot` already takes a
   snapDir — add a "fork N from snapshot" entry point that shares one
   read-only mem backing across N MAP_PRIVATE guests.

**Surface.** Internal perf first (TTI + pool-warm). Then `mvm fork <snapshot>
--count N` for the eval use case (Phase 4-adjacent).

**Risk.** This is the autoresearch domain — gate behind its correctness suite
(`go test ./internal/...` + exec/exit-code/file-roundtrip). UFFD MAP_PRIVATE
semantics need careful testing (dirty pages must not write back to pristine).

**Done when.** Pool-warm and TTI measurably drop on the bench (`score`
metric), with the correctness gate green. Target: TTI < 1s, pool refill no
longer serial.

---

## Phase 4 — Claude Managed Agents self-hosted backend  · effort L  *(distribution bet)*

**Learn from:** Anthropic Managed Agents self-hosted mode — an "environment
worker" polls a work queue; tool execution runs on your infra, only tool I/O
reaches Anthropic's control plane. Backends exist for Cloudflare/Daytona/
Modal/Vercel; **none is local-first + hardware-isolated.** This is reach, not
just a feature.

**Today.** mvm has the SDK + daemon API to create sandboxes and exec; nothing
speaks the managed-agents environment-worker protocol.

**Build.**
1. `cmd/mvm-agent-worker` (or `mvm worker`) — long-running process that
   registers with Anthropic's managed-agents control plane, polls the work
   queue, maps each tool-exec to `mvm exec` in a per-session sandbox, streams
   results back.
2. Session → sandbox lifecycle: create on session start, suspend on idle
   (Phase 1), checkpoint/restore for recovery, tear down on session end.
3. Wire egress policy (Phase 2) so the compliance story is end-to-end: code
   and credentials stay on the user's infra.

**Surface.** `mvm worker --anthropic-key … --pool 10`; a docs guide mirroring
Anthropic's other-backend guides.

**Risk.** Protocol is external and may change (verify current spec at
platform.claude.com before building; the research notes the public-beta
docs). Build behind a stable internal interface so we can also expose the same
worker for other harnesses (Claude Agent SDK, opencode's pluggable sandbox
layer).

**Done when.** A Claude managed agent runs a multi-tool task end-to-end with
execution confined to a local/self-hosted mvm sandbox, code never leaving the
host.

---

## Backlog (high value, lower urgency)

- **MCP server** *(microsandbox)* · S — `cmd/mvm-mcp` exposing
  create/exec/snapshot as MCP tools so any agent can drive mvm without the
  SDK. Cheap reach; pairs with Phase 4.
- **Declarative policy file** *(NVIDIA OpenShell)* · S–M — consolidate
  net/egress/seccomp/resources into a hot-reloadable `mvm.policy.yaml`. Natural
  home for Phase 2's egress+injection config; what a platform/compliance buyer
  wants in git. Replaces the ad-hoc `security.go` profiles.
- **Sandbox kits / presets** *(Docker "Sandbox Kits")* · S — `mvm start
  --kit claude-code|codex|cursor` over the existing `mvm build` image pipeline.
  DX win.
- **Parallel eval harness** *(Runloop)* · M — fork N sandboxes from one
  snapshot (Phase 3) to run SWE-bench/RFT locally or on one box; "evaluate your
  agent on your own hardware, free." No hosted vendor matches on cost.
- **Metrics / structured logging** · M — already roadmapped; needed before
  multi-tenancy and to substantiate the latency claims.

---

## Sequencing & rationale

```
Phase 1 (idle-to-zero) ─┐
Phase 2 (egress proxy) ─┼─► sharpen compliance + sustained-workload moat
Phase 3 (CoW fork)    ──┘   (perf; lands via autoresearch loop)
            │
            └─► Phase 4 (Anthropic backend) — distribution, depends on 1+2
                         └─► Backlog: MCP, policy.yaml, kits, eval harness
```

**Do Phase 1 and Phase 2 first** — both productize primitives mvm already has,
and both deepen the buyer (compliance / cost) that gVisor-class competitors
can't serve. **Phase 3** is the right next perf project and slots straight
into the autoresearch harness (it targets `score` via TTI + serial refill).
**Phase 4** is the upside bet — it depends on idle (1) and egress (2) for the
"code never leaves your infra" guarantee that is the whole reason a managed-
agents user would pick mvm over Anthropic's own cloud sandbox.

## Strategic risks the plan hedges

- **Docker Sandboxes** (cross-platform microVM, local+server, proprietary) is
  the most direct threat → counter with OSS + true self-hostable fleet +
  Phase 4 distribution.
- **microsandbox/SmolVM (libkrun)** could add a cloud story and collapse the
  "same API local+self-hosted" wedge → Phases 1–2 build switching cost
  (persistence + host-side security) beyond raw isolation.
- **gVisor "good enough"** (Modal, ~$4.65B) means many buyers won't pay for a
  hardware boundary → the plan deliberately aims at security/compliance/cost
  buyers, not latency-only ones.
