# Plan: closing the workdir gap (and pressing the Mac advantage)

Source: competitive teardown of `mv37-org/workdir` (June 2026). This sequences
six capabilities from workdir into mvm, ordered by value × effort and by
dependency. VZ-first (our differentiator), Firecracker following where cheap.

**Progress (2026-06-25):**
- **Phase 0 — DONE.** Boot-path telemetry + `mvm bench` shipped (`--json` on
  start, per-phase timing, `mvm bench` p50/p90/p95). Verified for `cold_boot`
  (635ms p50). Telemetry immediately surfaced that the base.ext4→rootfs copy is
  ~261ms of a ~697ms cold boot — a reflink/CoW optimization target.
- **Phase 3 — DONE (applevz).** Encrypted secrets: `mvm secret put/list/rm`
  (AES-256-GCM, key out of the store), attach with `mvm start --secret NAME`,
  injected per-exec from host memory (never to a guest file), and memory
  snapshots refused while a secret is attached. Verified end to end incl. no
  plaintext on disk + snapshot refusal.
- **Phase 2 — DONE (applevz).** `mvm start --startup recipe.json`: declarative
  git clone + env + foreground/background commands + in-guest ready-check, each
  phase timed into the BootResult. Verified: env injection, foreground exit-code
  checks, and detached background commands (a `sleep 120` survives start return).
- **Phase 1 — architect-reviewed, BLOCKED.** Design corrected below. Blocked by:

> ### ⚠️ BLOCKER: VZ save/restore regressed on macOS 26.2
> The identical save→stop→restore flow that measured ~0.29s and passed 8/8
> earlier in this session now fails **consistently** with
> `VZError code 12 "permission denied"` (`read mvm-vz status: EOF`). The helper
> binary is unchanged (ad-hoc signed, `com.apple.security.virtualization`,
> built 21 Jun); only the OS moved (now **26.2 / 25C56**). The saved state
> carries a system `com.apple.provenance` xattr that cannot be stripped. This is
> independent of the Phase 0 Go changes (pure instrumentation), but it **blocks
> Phase 1** (standby depends on restore) **and contradicts the ~0.29s restore
> claim now live in the README/landing page.** Must be diagnosed first —
> likely macOS 26.2 hardening of VZ restore around code identity; candidate
> fixes: sign the helper with a real Developer ID + provisioning profile bearing
> the virtualization entitlement (instead of ad-hoc), or capture the underlying
> Console error for the real reason.

## Guiding principles

- **Measure before tuning.** Phase 0 ships boot-path telemetry so every later
  phase is judged against real numbers — not the artifact that bit us last week.
- **VZ-first.** Our unmatched claim is native macOS isolation. Land each feature
  on the Apple VZ backend first; bring Firecracker along when the seam is shared.
- **No regressions to the cheap default.** A bare `mvm start` / `mvm exec` stays
  exactly as fast and simple as today; everything new is opt-in.
- **Don't cede "honesty."** workdir markets transparent boot paths. We match it
  and label our caveats as features.

## What mvm already has (the head start)

- `state.VM` carries `Status` (`running`/`paused`/`stopped`), `LastActivity`,
  `IdleTimeout`, `Backend`, `UFFDPid` (`internal/state/store.go`).
- VZ save/restore + disk rollback, verified ~0.29s (`runStartAppleVZ` in
  `internal/cli/start.go`, `vzBackend.SaveVM`, `rootfs.snapshot.ext4`).
- An idle checker that pauses VMs (`internal/cli/idle.go`, launchd-driven).
- `AutoResumeIfPaused` already wakes a paused VM in the exec path
  (`internal/cli/exec.go:112`). This is the seam standby reuses.
- Port forwarding (`state.PortMap`), per-VM network policy, custom images,
  snapshot encryption (`MVM_SNAPSHOT_KEY`).

So standby is "extend the idle reaper + the resume seam," not a rewrite.

---

## Phase 0 — Boot-path telemetry + `mvm bench`  (foundation)

**Value: high. Effort: low–medium. Do first.**

**Why.** Everything downstream needs a measured baseline, and an honest
per-path latency table is itself a marketing asset (workdir's headline). It also
permanently fixes the benchmarking pain from last week.

**Design.**
- Introduce a `BootPath` enum: `cold_boot` | `snapshot_restore` | `pool_claim`.
  `runStartAppleVZ` already knows which branch it took (the `restoreFrom != ""`
  test); thread that out instead of discarding it.
- Add a `StartResult` struct with `BootPath`, total wall time, and a per-phase
  breakdown (allocate → spawn helper → agent-ready → net-setup). We already
  print these stages; just time and capture them.
- `mvm start --json` emits the structured result (today it's `fmt.Printf` only,
  `start.go:145+`). This is also what the SDKs should return.
- New `mvm bench [--image X] [--samples N] [--json]` command: drives throwaway
  VMs through each boot path (cold, restore, pool), records p50/p90/p95, never
  writes a billable/durable VM record. Mirrors workdir's `src/bench.rs`. Reuses
  the file-redirect + `Popen.wait`-equivalent discipline (no pipe-EOF trap).

**Files.** `internal/cli/start.go` (result plumbing), new
`internal/cli/bench.go`, `internal/state` (BootPath const), SDKs later.

**Risks.** Low. Pure additive instrumentation. The one subtlety is timing the
agent-ready wait without counting the 60s timeout on failures — bound it.

---

## Phase 1 — Perpetual standby  (the headline feature)

**Value: high. Effort: medium–high. Blocked on the VZ-restore regression above.**

The "idle → $0 → auto-resume" model. mvm has the primitives (VZ save/restore,
idle checker, a resume seam). **Architect review corrected three wrong/under-
specified assumptions in the original sketch** — incorporated here.

**State machine.** Statuses: `running` ⇄ `paused` (helper alive, RAM resident,
instant) ⇄ **`standby`** (NEW: helper *killed*, RAM *freed*, `state.vzvmsave` +
`rootfs.snapshot.ext4` on disk, `$0`) ⇄ `stopped` (explicit). Key invariants:
- **Retain `NetIndex` in the record while standby** so `AllocateNet` recomputes
  the same IP/MAC on restore. The IP is logically free but reserved — standby
  VMs still count against the 62-VM ceiling. Correct and necessary.
- **Standby = `SaveVM` → `StopVM` + `waitForExit`.** `SaveVM` alone leaves the
  helper paused holding the disk lock; you must kill it and wait for the lock to
  release before flipping to `standby`. Disk-lock release is load-bearing for the
  next restore.

**The resume seam (corrected).** The original plan said "call the existing
restore path" — but `runStartAppleVZ` is a CLI body that prints to stdout and
**calls `RemoveVM` on every error path**; calling it from exec would delete the
VM record under an active request. Fix:
- Extract a non-destructive `restoreAppleVZ(store, *state.VM)` from the inline
  restore block in `start.go` (never removes the record; returns errors raw).
- Replace `AutoResumeIfPaused` with `ensureRunning(store, vm)` that dispatches on
  status: `running`→noop, `paused`+live-helper→`helper.Resume`,
  `paused`+dead-helper (post-reboot)→restore fallback, `standby`→`restoreAppleVZ`.
  (`paused` resume against a dead socket is a pre-existing latent bug standby
  surfaces — fix it here.) No import cycles: all in package `cli`.
- Widen the exec status guard (`exec.go:105`) to permit `standby`; both exec call
  sites (`:114`, `:147`) call `ensureRunning` and **propagate its error**.

**Race guards (the dangerous part — timestamps are insufficient).**
- **Reaper vs active request:** the reaper collects-then-acts with the lock
  released between phases, so it can standby a VM mid-exec. Guard: a **CAS on
  status inside the flock** — reaper flips `running→restoring` transactionally
  and only proceeds if the CAS wins; exec CAS-bumps activity *before* the long op.
- **Concurrent resume of one standby VM:** two execs both restore → two helpers
  fight the disk lock. Guard: CAS `standby→restoring` in a single `Transact`
  before spawning; the loser waits-and-polls for `running`.
- **Restart durability:** saved state/disk-snapshot/record survive on disk
  (only `delete` removes them); reaper skips non-`running`; first-touch restores.
  Confirmed safe once the dead-helper `paused` fallback above is in.

**Files.** `internal/state/store.go` (add `standby`/`restoring` status consts),
`internal/cli/start.go` (extract `restoreAppleVZ`, make the existing-VM guard
status-aware), `internal/cli/idle.go` (reaper → standby + `ensureRunning`),
`internal/cli/exec.go` (guard + propagate), `internal/cli/snapshot.go` (factor a
shared save+disk-clone helper to avoid drift with the reaper). Tests: non-
destructive restore, reaper CAS skip, single-helper concurrent resume, durability.

**Open decisions:** one-tier (idle→standby) vs two-tier (idle→pause→standby) —
recommend one-tier first; transient status name (`restoring`); whether `mvm list`
must read local state for applevz so standby shows correctly.

---

## Phase 2 — Declarative startup recipe

**Value: medium–high. Effort: low–medium. Independent.**

**Why.** Great agent ergonomics: one `create` that clones a repo, installs,
starts a dev server, and waits until it's healthy — each phase timed (feeds
Phase 0 telemetry).

**Design.**
- A `--startup <file.json>` (or SDK `startup:` field): `{ git:{url,ref},
  commands:[{name,run,background?}], ports:[…], ready:{http,timeout} }`.
- Runs after agent-ready via the existing `agent.Exec`; background commands via
  the PTY/exec-detached path. Ready-check polls an in-guest HTTP URL through the
  agent (workdir does this guest-side to avoid host networking).
- Each phase timed separately and returned in `StartResult`.

**Files.** New `internal/cli/startup.go`, `internal/agentclient` (already has
exec), `start.go` wiring.

**Risks.** Low. Shell-injection on git URLs / commands — single-quote escape like
workdir does; reuse mvm's existing `state.ValidateName`-style guards.

---

## Phase 3 — Encrypted, never-snapshotted secrets

**Value: medium. Effort: low–medium. Independent.**

**Why.** Agents need API keys without leaking them into snapshots. Clean,
self-contained, directly portable from workdir's design.

**Design.**
- Org/host-scoped secret store, AES-256-GCM, key kept out of the store
  (env `MVM_SECRET_KEY` or `0600` keyfile under `~/.mvm`). We already have the
  AES-GCM machinery from `MVM_SNAPSHOT_KEY`.
- `mvm secret put/list/rm` (names only ever returned).
- Inject **per-exec from host memory** into the agent's exec env — never written
  to a guest file. Therefore **refuse `snapshot`/standby while secrets are
  resident** (return an error), matching workdir's `409`.

**Files.** New `internal/cli/secret.go`, `internal/secrets/`,
`internal/agentclient` exec-env plumbing, a guard in `snapshot.go` + the standby
reaper.

**Risks.** Low–medium. The "refuse snapshot while secrets resident" rule
interacts with Phase 1 standby — sequence Phase 3 to land aware of it.

---

## Phase 4 — Authenticated preview-port proxy

**Value: medium. Effort: medium. Independent.**

**Why.** "Give me a public URL for the dev server in my sandbox" — a core agent
flow. mvm has port forwarding; this adds an authenticated front door.

**Design.**
- A small host-side reverse proxy: `https://<host>/p/<vm>/<port>/…` →
  guest `IP:port`. Auth via the existing API key.
- Copy workdir's *hardening verbatim* (their `REVIEW.md` documents the bugs):
  strip the auth token before forwarding upstream; only forward ports the VM
  actually exposed; refuse the control-plane port; uniform 404 for
  unauthorized/nonexistent (no existence leak).

**Files.** New `internal/server/preview.go` (daemon mode), route registration in
`internal/server/routes.go`.

**Risks.** Medium — it's an inbound proxy to untrusted guests; SSRF is the whole
risk surface. The mitigations are known because workdir already paid for them.

---

## Phase 5 — Restore-latency tuning (diff snapshots / background prewarm)

**Value: medium (lower for VZ). Effort: medium. Do last.**

**Why.** workdir cut resume 252ms→32ms by moving page-cache prewarm off the
critical path + diff snapshots — *without* UFFD. mvm's VZ restore is already
~0.29s; the bigger win here is on the **Firecracker** backend (UFFD already
shipped; diff snapshots + background prewarm would compound).

**Design.**
- Firecracker: enable diff snapshots (dirty-page tracking) for re-standby; move
  any eager prewarm to a background task off the resume path.
- VZ: investigate whether Virtualization.framework's restore exposes any
  equivalent lever; if not, accept 0.29s (it already beats workdir's cold boot).

**Files.** `internal/firecracker/*` (snapshot path), `internal/uffd/`.

**Risks.** Medium, and partly bounded by what the VZ API exposes (less knobs than
Firecracker). Lowest priority precisely because our number is already good.

---

## Recommended sequence & rough sizing

| # | Phase | Value | Effort | Depends on |
|---|-------|-------|--------|-----------|
| 0 | Boot-path telemetry + `mvm bench` | High | Low–Med | — |
| 1 | Perpetual standby (VZ) | High | Med–High | 0 (to measure) |
| 2 | Startup recipe | Med–High | Low–Med | — |
| 3 | Secrets (encrypted, never-snapshotted) | Med | Low–Med | aware of 1 |
| 4 | Preview proxy | Med | Med | — |
| 5 | Restore tuning (FC diff/prewarm) | Med | Med | 0 |

**Do 0 and 1 first** — telemetry then the headline feature. 2/3/4 are
independent and can be parallelized or slotted by appetite. 5 is a polish pass.

## Cross-cutting decisions to settle (architect review)

1. **Backend scope per phase.** VZ-first is assumed; confirm we're fine shipping
   standby/telemetry on VZ before Firecracker parity.
2. **Daemon vs direct-CLI.** Preview proxy and `mvm bench` sweep want the daemon;
   VZ today runs partly direct-CLI. Decide whether these features require
   `mvm serve`.
3. **Standby tiers.** One tier (pause→standby) to start, or two (pause →
   balloon → standby) like workdir? Recommend one tier first.
4. **License/positioning.** Keep leaning Apache-2.0 + native-Mac; none of this
   changes that, but standby + telemetry are the features that close the
   platform gap while we press the isolation advantage.
