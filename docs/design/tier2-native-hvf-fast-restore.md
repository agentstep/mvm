# Design Doc: Native Apple-Silicon Fast Restore (Tier 2)

**Status:** Draft for architect review
**Author:** (drafted with Claude, pending owner review)
**Date:** 2026-06-13
**Audience:** mvm maintainers + architecture review
**Decision sought:** Which restore architecture mvm should pursue to be the fastest microVM on Apple Silicon — and what to build first.

> **Tier model.** This is a two-tier performance effort. **Tier 1** = incremental wins inside the existing Firecracker path (reflink clones, poll tightening, UFFD-backed lazy restore) — these land on the **cloud Linux** deployment. **Tier 2** (this doc) = a native Apple-Silicon fast-restore path, because Tier 1 cannot reach the Mac (see §1). They are independent and ship separately.
>
> **Glossary (used throughout).** *UFFD* = Linux `userfaultfd`, the demand-paging mechanism Firecracker uses to lazily fault guest RAM in on restore. *VZ* = Apple's high-level Virtualization.framework. *HVF* = Apple's low-level Hypervisor.framework. *REAP* (ASPLOS '21) and *FaaSnap* (EuroSys '22) = research systems that make VM snapshot-restore fast by prefetching/serving only the working set of pages on demand — the model Option B would port to macOS. *Lima/Tart/UTM/krunkit* = existing macOS VM tools (container-dev VM, CI VM manager, desktop VM app, and libkrun's macOS wrapper, respectively).

---

## 1. Problem

On macOS, mvm's default path runs Firecracker **nested** inside a Lima Linux VM (`macOS → Virtualization.framework → nested KVM → Firecracker`). This is the only way to run Firecracker on a Mac (it needs `/dev/kvm`), and it carries two structural costs:

- **A nesting tax**: an independent M4 benchmark measured nested virt at ~73% of single-layer single-thread CPU and **up to ~70% fewer IOPS** (8,647 → 2,606), and nesting is only available at all on M3+/macOS 15+ ([UTM #6860](https://github.com/utmapp/UTM/discussions/6860), [Lima #2824](https://github.com/lima-vm/lima/issues/2824)).
- **Tier 1 wins don't reach the Mac**: reflink clones need a CoW filesystem (xfs/btrfs) and UFFD is a Linux-host mechanism — both apply to the cloud Linux host, not the nested-Mac path where Lima's guest is ext4 and the nesting tax dominates.

Meanwhile the only true *native* microVM on Apple Silicon — libkrun, on Hypervisor.framework — runs single-layer with no nesting tax. And per our competitive survey (§9), **no vendor publishes a methodology-backed microVM restore number on Apple Silicon**: Firecracker's 125ms is Linux-only, the cloud-sandbox sub-200ms figures are Linux-server snapshot-restore, and the VZ-based Mac tools publish no restore latency at all. The most visible *comparable* number is Apple's own `container` at **~0.9s warm launch** ([independent M3/M4 benchmarks](https://github.com/zot24/macos-container-benchmarks)). So the bar to beat is concrete (~0.9s) and the "fastest, honestly measured" position is genuinely open.

This doc decides how mvm should get there.

## 2. Goals / Non-goals

**Goals**
- A native (non-nested) fast-start path on Apple Silicon that beats the visible bar (~0.9s warm `container`) and an honest measurement of libkrun.
- **Success criteria, by use case** (these are different numbers — see §6): *warm-idle resume* (a sandbox we kept around) **< 200ms**; *cold start of a new sandbox* **< 900ms** to beat `container`, with **< 100ms** as the stretch "fastest, unprecedented" target.
- Preserve mvm's invariants: per-sandbox hardware isolation, a guest agent reachable over vsock, a NAT NIC, the same Debian rootfs + `mvm-agent`.
- An honest, published M-series benchmark (chip / macOS version / warm-resume-vs-cold-restore / sample size).

**Non-goals**
- Changing the cloud Linux path (Firecracker + UFFD already gets sub-second there; Tier 1 sharpens it).
- Cross-host portable snapshots on the Mac path (VZ save files are host-locked; see §4).
- Beating Firecracker's Linux 125ms *on Linux* — that's not this doc.

## 3. Where we are today (current code)

The Apple VZ backend already exists and is further along than its reputation:

- **`internal/vm/applevz.go`** drives a Swift helper (`mvm-vz`, `vz/Sources/mvm-vz/`) that builds a `VZVirtualMachine` with: `VZLinuxBootLoader`, an ext4 `VZVirtioBlockDevice`, `VZNATNetworkDeviceAttachment`, a `VZVirtioSocketDevice` (vsock), entropy + memory-balloon, and a serial console.
- **Pause/resume is fully implemented** in the helper (`ManagedVM.pause()`/`resume()` → `machine.pause`/`resume`, exposed over the `vzhelper` IPC as `CmdPause`/`CmdResume`) — but it is **CLI-blocked** by an outdated guard in `internal/cli/pause.go` that says pause "requires Firecracker's snapshot support (M3+)".
- The helper's state machine **already recognizes `saving` and `restoring`** states (`ManagedVM.swift`), but **no save/restore is wired** — nothing calls `saveMachineStateTo(url:)`/`restoreMachineStateFrom(url:)`.
- Same rootfs + `mvm-agent` as Firecracker; guest reached via vsock fd passed over SCM_RIGHTS (`internal/agentclient/dial_vz.go`).

So the native backend boots and runs; what's missing is fast *restore*.

## 4. The constraint landscape (researched, cited in Appendix)

What is actually possible for fast restore on Apple Silicon, from primary sources:

| Mechanism | Available on Apple Silicon? | Restore model | Notes |
|---|---|---|---|
| **VZ `saveMachineStateTo` / `restoreMachineStateFrom`** | Yes, macOS 14+ | **Eager** (full memory reconstituted) | Snapshot file is **hardware-encrypted to the originating Mac + user**; must pause before save; **no lazy/demand-paging hook**. |
| **HVF `hv_vm_protect` write-fault trapping** | Yes (proven: QEMU merged arm64 dirty-tracking on it, Jan 2026) | Enables **lazy / demand-paged** restore in a *custom* VMM | No built-in dirty bitmap; `ISV=0` instructions (LDP/STP/SIMD) need a TCG-style fallback for true demand paging. |
| **libkrun snapshot/pause/resume** | **No** (maintainer: "no use case"; dead code only) | — | krunkit likewise has none. |
| **QEMU `savevm`/migrate under `-accel hvf`** | **No** (architectural limitation, upstream-confirmed) | — | Works only under TCG (software emulation, slow). |

**Prior art for VZ Linux-guest save/restore is thin and unproven:** Tart ships VZ suspend but **excludes Linux guests**; UTM's VZ save-state is **broken** with open bugs; Lima's Linux VZ-suspend PR (#2900) is a **draft**, aarch64-only, reporting 37s→13s boot. Apple's docs say Linux guests *are* supported by the API, but nobody has shipped it in production.

**Two hard risks to flag loudly:**
1. **Host-locked snapshots.** VZ save files are hardware-encrypted to the originating Mac + user account, so we can't pre-bake one golden snapshot and distribute it — every Mac creates its own on first run. **Position: this is acceptable for the primary use case** (local dev — each developer's machine builds its snapshot once, exactly as the cloud path already cold-boots-then-snapshots per host). It only becomes a blocker if we later want shippable prebaked snapshots or fresh-VM-per-CI-job with no first-run cost; we accept the per-machine first-run cost and revisit only if a distribution use case appears (tracked as §8 Q4, not a launch blocker).
2. **Our current device set will likely fail validation as-is.** VZ exposes `VZVirtualMachineConfiguration.validateSaveRestoreSupport()` — call it to know definitively. Community-confirmed **incompatible** devices are: Virtio GPU, the **sound device**, the **entropy device**, and **console devices**. Our `mvm-vz` config currently builds **both an entropy device and a serial console** (`vz/Sources/mvm-vz/VM/VMManager.swift`) — so save/restore will probably reject it until those are removed or made conditional. By contrast, **vsock and the NAT NIC are *not* confirmed blockers** (no documented or community evidence either way — low confidence they block; runtime `validateSaveRestoreSupport()` is the arbiter). This reshapes the P1 spike: step one is dropping entropy+console and re-validating. Note also the state-machine requirement: the VM must be `.stopped` before `restoreMachineStateFrom` (success → `.paused`, then `resume()`), and `.paused` before `saveMachineStateTo`.

## 5. Options

### Option A — VZ native save/restore (eager, closed API)
Wire `saveMachineStateTo(url:)`/`restoreMachineStateFrom(url:)` into the existing helper; add `CmdSave`/`CmdRestore` to the `vzhelper` protocol; create a per-machine pristine snapshot on first run; restore from it on start.

- **Lift:** Small–medium. The helper already has the state machine and lifecycle plumbing.
- **Restore latency (estimate):** Eager — proportional to memory image size over local disk bandwidth. For a 2GB VM, low single-digit seconds, not sub-100ms.
- **Does it meet the goal? Partly — and this is the key nuance.** Eager save/restore at ~2–3s **does not beat the 0.9s cold-start bar.** So Option A is *not* the headline-latency play; its value is **cross-restart persistence** (survive a daemon/host reboot without a cold boot) and being faster than a true cold boot. The sub-200ms *warm* goal is met by **pause/resume (in-memory), not save/restore** — that's why Option C leads with pause/resume. The <900ms cold-start and <100ms stretch goals are only reachable via Option B. In short: pause/resume hits the warm bar today; save/restore is a persistence feature, not a latency win; Option B is the only thing that beats `container` on cold start.
- **Risks:** Host-locked snapshots (§4.1); Linux-guest VZ save/restore unproven (§4); device-compat unknown with vsock+NAT (§4.2). If the device-compat spike fails, this option is dead.

### Option B — Custom HVF VMM with demand-paged restore (REAP/FaaSnap-style)
Build (or fork) a minimal HVF-based VMM, map guest memory `READ|EXEC`, and demand-page from the snapshot on stage-2 faults — the only path to sub-100ms *lazy* restore on Apple Silicon.

- **Lift:** Large. A from-scratch VMM (vCPU loop, device models, virtio, vsock, networking) plus the demand-paging fault handler, including the `ISV=0` TCG fallback. libkrun is the closest base but has no snapshot scaffolding to build on.
- **Restore latency (target):** Sub-100ms achievable in principle (resume vCPUs immediately; pages fault in lazily) — this is the "fastest on Apple Silicon, unprecedented" outcome.
- **Risks:** Highest. Months of VMM work; ongoing maintenance burden of a hand-rolled hypervisor; correctness/security surface of a custom VMM; `ISV=0` emulation complexity.

### Option C — Hybrid / staged (recommended)
1. **Now:** Unblock the already-implemented VZ pause/resume (delete the stale CLI guard) → warm-idle VMs resume instantly in-memory. Near-zero lift, immediate win for the "keep N sandboxes warm" use case.
2. **Next:** Spike VZ save/restore device-compatibility (Option A) behind a flag. If it works with vsock+NAT, ship per-machine snapshot restore as the default Mac fast-start. If it doesn't, we've spent days, not months.
3. **Bet (research track):** Prototype the Option B demand-paging fault handler in isolation (map a memory image, trap writes via `hv_vm_protect`, measure fault-in latency) to validate the sub-100ms claim before committing to a full VMM.

## 6. Recommendation

**Pursue Option C.** It de-risks in the right order: a free win this week (pause/resume), a cheap go/no-go spike on VZ save/restore (the device-compat question is the whole ballgame), and a contained research prototype to decide whether the Option B moonshot is real before betting months on it. Commit to Option B only if (a) the HVF demand-paging prototype shows sub-100ms fault-in, and (b) VZ save/restore proves insufficient or device-incompatible.

The cloud Linux path stays on Firecracker+UFFD (Tier 1) — unchanged.

## 7. Phased plan

- **P0 — Unblock pause/resume (days).** Remove the `applevz` guard in `internal/cli/pause.go`; route pause/resume through `vzhelper`; integration-test idle-pause + resume-on-exec on the VZ backend. *Already-built code; just unblock + test.*
- **P1 — VZ save/restore spike (days).** Call `validateSaveRestoreSupport()` first; remove the entropy device + serial console from the config (both are confirmed-incompatible) and re-validate with vsock + NAT still present; then add `CmdSave`/`CmdRestore` and attempt a real save→stop→restore→resume cycle. **Decision gate:** does validation pass with vsock+NAT, and does the guest agent reconnect over vsock after restore? Measure restore latency for a 2GB VM.
- **P2 — Per-machine pristine snapshot + restore-on-start (1–2 wks).** If P1 passes: first-run creates the snapshot; `mvm start` restores from it; benchmark against cold boot and Apple `container`.
- **P3 — HVF demand-paging prototype (research, 1–2 wks).** Standalone: map an image, write-protect, trap faults, measure fault-in. **Decision gate:** sub-100ms feasible? Only then scope a full Option B VMM.
- **Throughout — Honest M-series benchmark.** Extend the autoresearch harness to record native-VZ numbers (chip, macOS version, cold vs pause-resume vs save-restore, n≥5).

## 8. Open questions

1. **Does VZ save/restore work with `VZVirtioSocketDevice` + `VZNATNetworkDeviceAttachment`?** (P1 answers this; it gates Option A.)
2. After `restoreMachineStateFrom`, does the **vsock device reconnect** cleanly, or does the agent need a reconnect handshake (as Cloud Hypervisor needs a vsock reset on restore)?
3. Per-machine snapshot creation adds first-run cost — acceptable, or do we need a faster first-run path?
4. Is the encrypted, host-locked snapshot a problem for any distribution/CI use case we care about?
5. Security review of a custom HVF VMM (Option B) — is that surface acceptable vs. Apple-maintained VZ?

## 9. Appendix: sources

- Apple VZ save/restore: WWDC23 "Create seamless experiences with Virtualization"; `VZVirtualMachine.saveMachineStateTo` / `restoreMachineStateFrom` (Apple Developer docs, macOS 14).
- VZ snapshots host-locked/encrypted; Linux-guest support claimed but unshipped: Tart `Run.swift` (macOS-guest-only `PlatformSuspendable`); UTM issues #6654/#4172/#7326 (VZ save broken); Lima PR #2900 (draft, 37s→13s).
- HVF write-fault trapping for dirty tracking / demand paging: QEMU arm64 HVF dirty-tracking merged Jan 2026 (`hvf_protect_clean_range`/`hvf_unprotect_dirty_range`); `hv_vm_protect` Apple docs; `hv_vcpu_exit_exception_t.syndrome` (ESR_EL2) + `.physical_address` (HPFAR_EL2); ISV=0 TCG-fallback RFC (2025).
- libkrun no snapshot/pause/resume: libkrun issue #67 (maintainer "no use case"); public C API audit (no `krun_pause`/`krun_snapshot`); krunkit REST API (Running/Stopped only).
- QEMU savevm/migrate unsupported under HVF on Apple Silicon: QEMU HVF arm64 patch series (A. Graf), upstream-confirmed.
- Current mvm code: `internal/vm/applevz.go`, `internal/vzhelper/{client,protocol}.go`, `vz/Sources/mvm-vz/`, `internal/cli/pause.go`, `internal/agentclient/dial_vz.go`.
