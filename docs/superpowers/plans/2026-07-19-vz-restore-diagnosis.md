# VZ save/restore regression (macOS 26.2) — Diagnosis & Graceful-Degradation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Phase 1 (Tasks 1–2) is a DIAGNOSTIC RUNBOOK: run the exact commands, read the output, and follow the named decision branch. Phase 2 (Tasks 3–4) is CONDITIONAL — do exactly one of the two, selected by Task 2's recorded outcome. Phase 3 (Task 5) ships regardless. Do not write speculative fixes ahead of the diagnosis that names them.

**Goal:** Determine, on the *current* macOS build, whether `restoreMachineStateFrom` still fails with `VZError 12 "permission denied"`, record the true underlying cause in a durable findings artifact, then either re-verify the 0.29s restore claim (if fixed) or file a scoped OS-level bug and ship a runtime graceful-degradation fallback so `mvm start`-from-checkpoint degrades to cold-boot-from-checkpoint-disk instead of hard-erroring (if still broken).

**Architecture:** On the applevz backend there is *no* separate "restore" command. Restore is **restore-on-start**: `runStartAppleVZ` (`internal/cli/start.go:221`) checks for `~/.mvm/vms/<name>/state.vzvmsave`; if present it sets `restoreFrom` and passes it to `AppleVZBackend.StartVM` (`internal/vm/applevz.go:105`), which spawns the `mvm-vz` Swift helper with `--restore-from`. The helper (`vz/Sources/mvm-vz/Commands/Create.swift:111-137`) calls `machine.restoreMachineStateFrom(url:)`. Save is the mirror: `mvm snapshot create` → `runSnapshotCreate` (`internal/cli/snapshot.go:86`) → `AppleVZBackend.SaveVM` → helper `saveMachineStateTo` (`vz/Sources/mvm-vz/VM/ManagedVM.swift:111`). `mvm bench` (`internal/cli/bench.go`) drives the whole checkpoint→stop→restore loop in-process and is the fastest aggregate reproducer.

**Tech Stack:** Go 1.22+ (`internal/cli`, `internal/vm`, `internal/vzhelper`), Swift Package Manager + Virtualization.framework (`vz/`, built via `make vz`), macOS `log`/`sw_vers`/`codesign` CLIs for OS-level diagnosis. Module path `github.com/agentstep/mvm`.

## Global Constraints

- **Run every command from `/Users/paulmeller/Projects/firecracker`.** All paths in this plan are absolute or repo-relative from there.
- **This is macOS-Apple-Silicon-only work.** Every task requires a machine where `go run ./cmd/mvm list` succeeds with the applevz backend initialized. If a task runs on a machine without an initialized applevz backend, STOP and record "no applevz host" in the findings file — do not fabricate results.
- **The diagnosis produces a durable artifact.** Findings live in `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md`. Every diagnostic step appends its verbatim command output there. Phase-2 task selection reads the `## VERDICT` line from that file — the fix tasks are conditional on a *named, written* outcome, never on memory.
- **No speculative source edits before diagnosis.** Tasks 1–4 touch only the findings file and the OS (build/run/log). Only Task 5 edits `.go`/`.swift` source, and its fallback is written so it is a *no-op when restore works* (it fires only on an actual restore error), so it ships regardless of the verdict.
- **Do not delete the README caveat until restore is verified working on the current build.** README.md:120-124 currently warns restore is broken on 26.2; removing it is a Task-3 (fixed-branch) action only.
- Match existing code style: tabs, stdlib-only tests, matching every other file in `internal/cli`.

### Known-facts reconciliation (READ BEFORE STARTING — the ground truth is ~3 weeks stale and the codebase already contradicts parts of it)

1. **"Nothing logs the EPERM's cause" is FALSE as of commit `c9af8bd` (2026-06-27).** `Create.swift:117-128` already catches the restore error, prints `domain=/code=/reason=`, and walks up to 5 levels of `NSUnderlyingErrorKey` to stderr. That stderr is captured to `~/.mvm/vms/<name>/mvm-vz-stderr.log` and folded into the Go-side error by `withHelperStderr` (`internal/vm/applevz.go:264`). So the underlying cause is *already recoverable* — Task 2 triggers it and reads the log rather than adding new logging.
2. **`mvm snapshot restore` does NOT exercise the applevz restore path.** `runSnapshotRestore` (`internal/cli/snapshot.go:138`) is unconditionally daemon/Firecracker-only (`requireDaemon()` at line 139, no applevz branch). The applevz "restore" is restore-on-start via `runStartAppleVZ`. The task brief's mention of an "`mvm snapshot restore` applevz path" does not exist — the graceful-degradation fallback therefore belongs in `runStartAppleVZ`, not in `runSnapshotRestore`. This is corrected throughout this plan.
3. **README's stated fix direction is contradicted by the newer notes.** README.md:123-124 says the fix in progress is "sign the helper with a Developer ID + provisioning profile." The 2026-06-26/27 notes say signing was *ruled out* (ad-hoc AND real "Apple Development" cert + hardened runtime both fail identically). Treat signing as a dead end already tested; Task 4 re-confirms it once, then moves on.
4. **The OS build has not moved.** At plan-authoring time `sw_vers` still reports `26.2 / 25C56` — identical to the build in the known-facts. Task 1 re-checks this on the machine actually running the tasks; if it is still 25C56, the "Apple may have fixed it in a point release" hypothesis is already unlikely but must still be tested empirically in Task 2.

---

### Task 1: Environment baseline & findings-artifact scaffold

**Files:**
- Create: `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md`

**Interfaces:**
- Produces: the findings file with a filled-in `## Environment` section and an empty `## VERDICT` line. Tasks 2–5 append to / read this file.

- [ ] **Step 1: Capture the environment**

Run:
```bash
sw_vers
uname -m
xcodebuild -version 2>/dev/null || echo "no full Xcode"
swift --version
codesign -d --entitlements :- bin/mvm-vz 2>/dev/null || echo "bin/mvm-vz not built yet"
```
Record each command's full output for Step 2.

- [ ] **Step 2: Write the findings scaffold**

Create `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md` with this exact skeleton, pasting the Step-1 output into `## Environment`:

```markdown
# VZ restore diagnosis — findings (2026-07-19)

## VERDICT
PENDING   <!-- Task 2 overwrites this line with exactly one of: RESTORE_WORKS | RESTORE_BROKEN -->

## Environment
- sw_vers: <paste ProductVersion + BuildVersion>
- arch: <paste uname -m — expect arm64>
- swift: <paste>
- helper entitlements: <paste, expect com.apple.security.virtualization = true>
- Baseline for comparison: macOS 26.2 build 25C56 (the build where restore was last observed broken, 2026-06-26/27).
- Restore last verified WORKING: 2026-06-21, 0.293s median (pre-26.2).

## Task 2 — reproduction
<!-- appended by Task 2 -->

## Task 4 — OS-level diagnosis
<!-- appended by Task 4, only if VERDICT=RESTORE_BROKEN -->
```

- [ ] **Step 3: Decision branch on build number**

- If `sw_vers` BuildVersion is **exactly `25C56`** → the OS is unchanged from the known-broken build. Restore is *expected* to still fail; proceed to Task 2 to confirm and capture the true cause. Note in the findings file: `Build unchanged (25C56) — upstream point-release fix is unlikely; confirming empirically.`
- If BuildVersion is **anything newer than `25C56`** (e.g. `25Cxx` > 56, or a `26.3`/`25Dxx` build) → Apple may have shipped a fix. Note in the findings file: `Build advanced to <X> — testing the upstream-fix hypothesis.` Proceed to Task 2.
- Either way, Task 2 is next. The build number only sets the *prior*, not the outcome.

- [ ] **Step 4: Commit the scaffold**

```bash
git add docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md
git commit -m "diag(vz-restore): capture environment baseline and findings scaffold"
```

---

### Task 2: End-to-end restore reproduction & underlying-cause capture (DECISION POINT)

**Files:**
- Modify: `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md`

**Interfaces:**
- Consumes: findings scaffold (Task 1).
- Produces: the `## VERDICT` line set to `RESTORE_WORKS` or `RESTORE_BROKEN`, and the verbatim `mvm-vz-stderr.log` restore-failure block (if any) under `## Task 2 — reproduction`.

- [ ] **Step 1: Rebuild the helper from current source (rules out a stale binary)**

Run:
```bash
make vz
codesign -d --entitlements :- bin/mvm-vz 2>&1 | grep -q virtualization && echo "SIGNED-OK" || echo "SIGN-MISSING"
```
Expected: `swift build` succeeds, `bin/mvm-vz` copied, `SIGNED-OK`. If `SIGN-MISSING`, stop — the `make vz` codesign step (`Makefile:20`) failed and any restore result would be meaningless.

- [ ] **Step 2: Confirm an applevz host is available**

Run:
```bash
go run ./cmd/mvm list
```
- If this errors with "not initialized" / no backend → STOP. Write `No applevz host — reproduction not possible on this machine.` into the findings file and escalate to the user; the rest of Phase 1–2 needs hardware.
- If it prints a VM list (possibly empty) without erroring → continue.

- [ ] **Step 3: Cold-boot a throwaway VM, checkpoint it, stop it**

Run each and confirm success before the next:
```bash
go run ./cmd/mvm delete diag-restore 2>/dev/null; true      # clean slate, ignore "not found"
rm -rf ~/.mvm/vms/diag-restore                              # ensure no stale state.vzvmsave
go run ./cmd/mvm start diag-restore
go run ./cmd/mvm snapshot create diag-restore
go run ./cmd/mvm stop diag-restore
```
Expected: `start` prints a running banner; `snapshot create` prints `✓ Checkpoint saved (memory + disk)` (this exercises `saveMachineStateTo` — per the known-facts, save WORKS, so this must succeed; if *save* fails, that is a new regression — record it and stop); `stop` succeeds. After this, `~/.mvm/vms/diag-restore/state.vzvmsave` and `rootfs.snapshot.ext4` exist:
```bash
ls -la ~/.mvm/vms/diag-restore/state.vzvmsave ~/.mvm/vms/diag-restore/rootfs.snapshot.ext4
```

- [ ] **Step 4: Attempt the restore (this is the whole experiment)**

`runStartAppleVZ` auto-detects the saved state and restores on the next start (`internal/cli/start.go:277-281`). Run:
```bash
go run ./cmd/mvm start diag-restore ; echo "EXIT=$?"
```
Then immediately capture the helper's own stderr (where `Create.swift:117-128` writes the walked error chain):
```bash
echo "----- mvm-vz-stderr.log -----"
cat ~/.mvm/vms/diag-restore/mvm-vz-stderr.log 2>/dev/null || echo "(no stderr log)"
echo "----- console.log tail -----"
tail -40 ~/.mvm/vms/diag-restore/console.log 2>/dev/null || echo "(no console log)"
```

- [ ] **Step 5: Classify the outcome and set the VERDICT**

Read the `mvm start` output and the stderr log from Step 4, and choose exactly one branch:

- **BRANCH A — RESTORE WORKS:** `mvm start diag-restore` prints the running banner with **`boot_path` shown as `snapshot_restore`** (or `--json` reports `"boot_path":"snapshot_restore"`), `EXIT=0`, the agent becomes reachable (`go run ./cmd/mvm exec diag-restore -- echo ok` prints `ok`), and `mvm-vz-stderr.log` contains **no** `restore failed:` line.
  - Set the findings `## VERDICT` line to `RESTORE_WORKS`.
  - Under `## Task 2 — reproduction`, paste the `boot_path` line and note the exact build it worked on.
  - **→ Go to Task 3. Skip Task 4.**

- **BRANCH B — RESTORE BROKEN:** `mvm start` errors (nonzero `EXIT`) and/or `mvm-vz-stderr.log` contains a `restore failed: domain=... code=... reason=...` line. The known signature is `code=12` (VZErrorRestore) wrapping a `permission denied` / `EPERM (1)` underlying error.
  - Set the findings `## VERDICT` line to `RESTORE_BROKEN`.
  - Under `## Task 2 — reproduction`, paste the FULL `restore failed:` block including every `underlying[n]:` line — this is the real cause the rest of the plan turns on. Explicitly record: the top-level `code`, and the deepest non-nil `underlying[n]` `domain`/`code`/`desc` (e.g. `NSPOSIXErrorDomain code=1` = EPERM, vs `code=13` = EACCES, vs a Virtualization-internal code — these point at different root causes).
  - **→ Go to Task 4. Skip Task 3.**

- [ ] **Step 6: Cross-check with the aggregate reproducer (both branches)**

Run:
```bash
go run ./cmd/mvm bench --samples 3 --json 2>/dev/null | python3 -m json.tool
```
`bench` runs `cold_boot` then `snapshot_restore` (`internal/cli/bench.go:88-121`). Record the `snapshot_restore` object's `samples`/`failed` counts in the findings file. Consistency check: Branch A expects `failed:0` with real `p50_ms`; Branch B expects `failed:3` (or `samples:0`). A mismatch between Step 5 and this aggregate is itself a finding — note it and trust the direct Step-4 evidence.

- [ ] **Step 7: Clean up and commit the findings**

```bash
go run ./cmd/mvm delete diag-restore 2>/dev/null; rm -rf ~/.mvm/vms/diag-restore
git add docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md
git commit -m "diag(vz-restore): record reproduction result and VERDICT"
```

---

### Task 3: [CONDITIONAL — only if VERDICT=RESTORE_WORKS] Re-verify the 0.29s claim and update docs

**Run this task only if `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md`'s `## VERDICT` line reads `RESTORE_WORKS`. Otherwise skip to Task 4.**

**Files:**
- Modify: `README.md`
- Modify: `docs/landing.md`
- Modify: `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md`

**Interfaces:**
- Consumes: `RESTORE_WORKS` verdict.
- Produces: an updated restore benchmark number and a README/landing without the "broken on 26.2" caveat.

- [ ] **Step 1: Re-measure the restore percentile with the real bench harness**

Run:
```bash
go run ./cmd/mvm bench --samples 8 --json 2>/dev/null | python3 -m json.tool | tee -a docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md
```
Expected: the `snapshot_restore` object has `failed:0` and 8 samples. Record its `p50_ms`, `min_ms`, `max_ms`. (`n=8` matches the original README methodology at README.md:110.)

- [ ] **Step 2: Decide whether the headline number still holds**

- If `p50_ms` ≤ ~350ms → the "~0.293s / ~0.3s" claim stands. Proceed to Step 3 to remove the caveat with the *newly measured* number.
- If `p50_ms` is materially higher (e.g. > 500ms) → restore works but is slower on this build. Do NOT restore the old number. Update the README figure to the measured value and note the regression-in-latency in the findings file. Proceed to Step 3 using the measured number.

- [ ] **Step 3: Remove the "broken on 26.2" caveat from README**

In `README.md`, delete the footnote block at lines 120-124 (the `> ¹ **Restore is currently broken on macOS 26.2.** …` paragraph) and the `¹` superscript markers on lines 115-116, and update the current-build note. Replace the caveat with a one-line verified-on note, e.g.:
```
> Restore verified working on macOS <BuildVersion from Task 1>: <p50 from Step 1>s median (n=8, zero failures).
```
Also update README.md:12's inline `note the macOS 26.2 caveat` comment to drop the caveat reference.

- [ ] **Step 4: Update the landing copy**

Run `grep -n "26.2\|broken\|permission denied\|0.29\|standby" docs/landing.md` and update any restore-caveat or restore-latency copy to match README (same measured number, caveat removed). If `grep` returns nothing, note "landing has no restore claim to update" in the findings file and move on.

- [ ] **Step 5: Re-enable perpetual-standby follow-up (scope check only)**

Perpetual-standby is a *planned* feature, not disabled code — confirm there is no `if false`/build-tag/feature-flag gate to flip:
```bash
grep -rn "perpetual\|standby\|TODO.*restore\|restore.*broken\|26\.2" internal/ cmd/ vz/Sources/
```
Record the hits in the findings file. If any code path is gated behind the restore regression (e.g. a disabled bench path or a skipped test), re-enable it here; if there are none (expected — it was never built), note "no disabled code to re-enable; perpetual-standby remains greenfield" and leave a pointer for the future feature plan.

- [ ] **Step 6: Verify build and commit**

```bash
go build ./... && go vet ./...
git add README.md docs/landing.md docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md
git commit -m "docs(vz-restore): restore verified working on current build; refresh benchmark, drop 26.2 caveat"
```
**Then proceed directly to Task 5** — the graceful-degradation fallback still ships (it is a runtime safety net for future OS regressions and for the still-broken-elsewhere case; it is a no-op when restore works).

---

### Task 4: [CONDITIONAL — only if VERDICT=RESTORE_BROKEN] Structured OS-level diagnosis & upstream report

**Run this task only if the `## VERDICT` line reads `RESTORE_BROKEN`. Otherwise skip (you came from Task 3).**

**Files:**
- Modify: `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md`
- Create (scratch, not committed): `/private/tmp/claude-501/-Users-paulmeller-Projects-firecracker/364cc379-e2b4-41f6-a1a1-c837badb13f1/scratchpad/vz-minirepro/` for any minimal Swift sample

**Interfaces:**
- Consumes: `RESTORE_BROKEN` verdict + the pasted `restore failed:` chain from Task 2.
- Produces: a `## Task 4 — OS-level diagnosis` section ending in a `### DECISION` block naming the root-cause category (one of: `OS_WIDE_VZ_BUG` | `MVM_DEVICE_SET` | `MVM_SIGNING` | `MVM_STATE_FILE`), which determines whether a code fix is even possible or the fallback (Task 5) is the only remedy.

- [ ] **Step 1: Capture unified-logging around a fresh restore attempt**

In terminal A, start streaming the Virtualization subsystem, then in terminal B re-run the repro. Terminal A:
```bash
log stream --style syslog --level debug \
  --predicate 'process == "mvm-vz" OR senderImagePath CONTAINS "Virtualization" OR subsystem CONTAINS[c] "virtualization"' \
  | tee /tmp/vz-restore-logstream.txt
```
Terminal B (recreate the checkpoint first, as Task 2 deleted it):
```bash
go run ./cmd/mvm start diag-restore && go run ./cmd/mvm snapshot create diag-restore && go run ./cmd/mvm stop diag-restore
go run ./cmd/mvm start diag-restore ; echo "EXIT=$?"
```
Stop terminal A (Ctrl-C). If `log stream` shows nothing useful, fall back to a retrospective query:
```bash
log show --last 5m --style syslog --info --debug \
  --predicate 'process == "mvm-vz" OR eventMessage CONTAINS[c] "restore" OR eventMessage CONTAINS[c] "sandbox"' | tail -100
```
Also check for a sandbox denial (a common source of EPERM that never reaches the app-level error):
```bash
log show --last 5m --predicate 'senderImagePath CONTAINS "Sandbox" AND eventMessage CONTAINS "mvm-vz"' 2>/dev/null | tail -40
```
Paste the most relevant 20-40 lines into the findings file under `## Task 4`. Decision seed: a `Sandbox: mvm-vz deny(1) file-read-data <state.vzvmsave path>` line → points at `MVM_STATE_FILE`/entitlement; a Virtualization-internal assertion with no sandbox line → points at `OS_WIDE_VZ_BUG`.

- [ ] **Step 2: Minimal-repro variation — isolate the device set**

The helper omits entropy/console/balloon in save/restore mode but keeps **network (NAT)** and **vsock** (`vz/Sources/mvm-vz/VM/VMManager.swift:71-104`). First confirm the config still passes VZ's own gate on this build:
```bash
./bin/mvm-vz validate-saverestore --kernel ~/.mvm/cache/vmlinux --rootfs ~/.mvm/cache/base.ext4
```
Expected (per prior spike): the `save/restore set` line reports `SUPPORTED ✅`. Record the actual output.
- If it now reports `REJECTED ❌` → the device set itself became invalid on this build → root-cause category `MVM_DEVICE_SET`; the fix is to trim the offending device (network or vsock) from the save/restore config in `VMConfigBuilder.build`. Record which device and stop the device-isolation sub-task.
- If it reports `SUPPORTED ✅` but restore still EPERMs → the failure is *not* validation; it is at `restoreMachineStateFrom` runtime. Continue to Step 3. (Note: `validate-saverestore` builds a config with `logPath:"/tmp/mvm-vsr-spike.log"` and no `--restore-from`, so it exercises validation only, not the restore syscall — that gap is exactly why Step 3 is needed.)

- [ ] **Step 3: Minimal-repro variation — signing, re-confirmed once**

The known-facts say signing was ruled out; re-confirm on the current build with the two extremes, then never revisit:
```bash
# (a) ad-hoc (current make vz default) — already the binary under test.
codesign -dv bin/mvm-vz 2>&1 | grep Signature
# (b) if a real "Apple Development" identity is available in the keychain:
security find-identity -v -p codesigning | grep "Apple Development" || echo "no dev identity — skip (b)"
```
If (b) has an identity, re-sign a copy and retry restore once:
```bash
cp bin/mvm-vz /tmp/mvm-vz-devsigned
codesign --force --sign "Apple Development" --options runtime \
  --entitlements vz/mvm-vz.entitlements /tmp/mvm-vz-devsigned
# Point the backend at it and retry (HelperBinary() picks bin/mvm-vz next to the exe; swap temporarily):
cp bin/mvm-vz /tmp/mvm-vz-adhoc-backup && cp /tmp/mvm-vz-devsigned bin/mvm-vz
go run ./cmd/mvm start diag-restore ; echo "DEVSIGN-EXIT=$?"
cp /tmp/mvm-vz-adhoc-backup bin/mvm-vz   # restore ad-hoc binary
```
Record the result. Expected per known-facts: identical EPERM → signing is NOT the cause → rule out `MVM_SIGNING`. If (b) unexpectedly *succeeds*, that flips the whole plan — set root-cause `MVM_SIGNING`, and Task 5's fallback becomes secondary to a signing fix in `Makefile:14-20`; record this prominently.

- [ ] **Step 4: Isolate mvm-specific vs OS-wide with an independent VZ restorer**

Determine whether *any* process can restore on this OS build, or only mvm fails:
```bash
# Apple's container tool ships a VZ-based runtime; if installed it is the fastest oracle:
which container && container --version || echo "no apple container tool"
```
- If `container` is present: create a container, checkpoint/restore it per its docs, and record whether *its* restore works. `container` restore works but mvm's fails → `MVM_DEVICE_SET`/`MVM_STATE_FILE` (mvm-specific). Both fail → `OS_WIDE_VZ_BUG`.
- If `container` is absent: build the ~40-line minimal Swift VZ sample in the scratchpad dir (`vz-minirepro/`) that boots a bare config — **storage + one boot loader only, no network, no vsock** — saves via `saveMachineStateTo`, then restores via `restoreMachineStateFrom`. Reuse the exact patterns from `Create.swift:107-137` and `ManagedVM.swift:111-129`. Sign it ad-hoc with `vz/mvm-vz.entitlements` and run save then restore. Record: does the *bare* config restore?
  - Bare config restores, mvm's does not → a device mvm keeps (network/vsock) is the trigger → `MVM_DEVICE_SET`. Confirm by adding devices back one at a time.
  - Bare config also EPERMs → `OS_WIDE_VZ_BUG`; no mvm-side code fix exists and Task 5's fallback is the *only* remedy until Apple ships a fix.

- [ ] **Step 5: File a Feedback Assistant report (only if root cause is `OS_WIDE_VZ_BUG`)**

If Step 4 concluded `OS_WIDE_VZ_BUG`, file at https://feedbackassistant.apple.com with: the `sw_vers` build, the minimal Swift repro from Step 4 (self-contained, no mvm dependency), the full `restore failed:` chain from Task 2, and the `log show` sandbox/Virtualization excerpt from Step 1. Record the Feedback ID (`FBxxxxxxx`) in the findings file. This is required so the eventual README claim can cite an upstream tracking bug rather than a bare "broken."

- [ ] **Step 6: Write the DECISION block and commit**

Append to the findings file:
```markdown
### DECISION
- Root-cause category: <OS_WIDE_VZ_BUG | MVM_DEVICE_SET | MVM_SIGNING | MVM_STATE_FILE>
- Evidence: <one line per: logstream, validate-saverestore, signing retry, independent-restorer>
- Code fix possible in mvm? <YES: describe the exact file+change | NO: OS-side, fallback is the only remedy>
- Feedback Assistant ID (if OS_WIDE): <FBxxxxxxx | n/a>
```
Then:
```bash
go run ./cmd/mvm delete diag-restore 2>/dev/null; rm -rf ~/.mvm/vms/diag-restore
git add docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md
git commit -m "diag(vz-restore): OS-level diagnosis, root-cause category, upstream report"
```
- If DECISION names an in-mvm code fix (`MVM_DEVICE_SET`/`MVM_STATE_FILE`/`MVM_SIGNING`), implement it as a small dedicated task *before* Task 5, guided by the recorded evidence (e.g. trim the offending device in `VMConfigBuilder.build`, or fix the state-file path/permissions). That fix's exact code is intentionally not pre-written here because it is unknowable until this diagnosis names it — the DECISION block IS its spec.
- Regardless of category, **proceed to Task 5** — the fallback ships either way.

---

### Task 5: [REGARDLESS] Graceful-degradation — cold-boot-from-checkpoint-disk fallback on restore failure

**Ships whether the verdict was RESTORE_WORKS or RESTORE_BROKEN.** When restore works, the fallback never fires (it is guarded on an actual restore error), so it is safe to ship unconditionally as a runtime safety net for future OS regressions.

**Files:**
- Modify: `internal/cli/bootresult.go`
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/bench.go`

**Interfaces:**
- Consumes: `AppleVZBackend.StartVM(name, kernelPath, rootfsPath, bootArgs, mac string, cpus, memoryMB int, volumes []string, restoreFrom string) (*StartResult, error)` (`internal/vm/applevz.go:105`) — unchanged.
- Produces: a new `BootPath` value `BootRestoreFallback` and fallback logic in `runStartAppleVZ` that retries cold when a restore-start errors.

- [ ] **Step 1: Add the new BootPath constant**

In `internal/cli/bootresult.go`, add to the `const (...)` block at lines 19-23, after `BootRestore`:
```go
	BootRestoreFallback BootPath = "restore_fallback_cold" // restore failed; cold-booted the checkpoint-rolled-back disk instead
```

- [ ] **Step 2: Wire the fallback into `runStartAppleVZ`**

In `internal/cli/start.go`, the restore-start is a single call at line 323:
```go
	startResult, err := vzBackend.StartVM(name, kernelPath, vmRootfs, bootArgs, alloc.GuestMAC, vzCpus, vzMem, volumes, restoreFrom)
	if err != nil {
		store.RemoveVM(name)
		return nil, fmt.Errorf("start VM: %w", err)
	}
```
Replace that block with the fallback-aware version. The disk was ALREADY rolled back to the checkpoint snapshot at start.go:285-293 (that happens whenever `restoreFrom != ""`), so a cold boot here recovers the checkpoint's *filesystem* state — losing only resident RAM, not disk. That is exactly the "cold boot + disk-state restore" degradation the plan calls for:
```go
	startResult, err := vzBackend.StartVM(name, kernelPath, vmRootfs, bootArgs, alloc.GuestMAC, vzCpus, vzMem, volumes, restoreFrom)
	if err != nil && restoreFrom != "" {
		// Graceful degradation: VZ restore can fail at runtime (observed as
		// VZError 12 "permission denied" on macOS 26.2 — see
		// docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md).
		// The rootfs was already rolled back to the checkpoint snapshot above
		// (see the restoreFrom disk-rollback block), so a cold boot from that
		// same disk recovers the checkpoint's filesystem state — everything but
		// resident RAM. Prefer that over hard-failing the start.
		logf("  Warning: restore failed (%v); falling back to cold boot from checkpoint disk\n", err)
		bootPath = BootRestoreFallback
		startResult, err = vzBackend.StartVM(name, kernelPath, vmRootfs, bootArgs, alloc.GuestMAC, vzCpus, vzMem, volumes, "")
	}
	if err != nil {
		store.RemoveVM(name)
		return nil, fmt.Errorf("start VM: %w", err)
	}
```
No other change is needed: `bootPath` already flows into the `BootResult` at start.go:455, so a fallback boot self-reports as `restore_fallback_cold`.

- [ ] **Step 3: Keep bench honest about the fallback**

`benchOneStart` (`internal/cli/bench.go:136`) currently counts any agent-ready start as a restore sample, checking only `res.AgentReady`. With the fallback active, a *failed* restore that degrades to cold boot would be silently counted as a successful `snapshot_restore` sample — publishing cold-boot latency under the restore headline, the exact dishonesty `BootPath` exists to prevent. Fix `benchOneStart` to surface the actual path so the restore loop can reject fallbacks. Change its signature and body:
```go
// benchOneStart starts the bench VM and returns its total boot time in ms and
// the path it actually took, counting it only if the start succeeded and the
// agent answered.
func benchOneStart(store *state.Store) (float64, BootPath, bool) {
	res, err := runStartAppleVZ(store, benchVMName, false, nil, "open", 0, 0, nil, outQuiet, nil, nil)
	if err != nil || res == nil || !res.AgentReady {
		return 0, "", false
	}
	return res.TotalMs, res.BootPath, true
}
```
Update the two call sites in `runBench`:
- cold_boot loop (bench.go:91): `ms, _, ok := benchOneStart(store)` (path unchecked for cold).
- the pre-checkpoint start (bench.go:104): `if _, _, ok := benchOneStart(store); ok {`.
- the restore loop (bench.go:110-116): reject a fallback as a failed restore sample:
```go
			ms, path, ok := benchOneStart(store)
			if ok && path == BootRestore {
				restore.RawMs = append(restore.RawMs, ms)
			} else {
				restore.Failed++
			}
```
This way, while restore is broken and the fallback fires, `mvm bench` reports `snapshot_restore` as all-failed (truthful) rather than laundering cold-boot numbers.

- [ ] **Step 4: Build, vet, and unit-test**

```bash
go build ./... && go vet ./... && go test ./internal/cli/ 2>&1 | tail -20
```
Expected: clean build, vet silent, package `ok`. (The fallback path itself needs hardware to exercise; the compile + existing tests guard the wiring.)

- [ ] **Step 5: Behavioral verification on hardware (if an applevz host is available)**

Only meaningful when restore is actually broken (VERDICT=RESTORE_BROKEN). Reproduce and confirm the degrade:
```bash
go run ./cmd/mvm delete fb 2>/dev/null; rm -rf ~/.mvm/vms/fb
go run ./cmd/mvm start fb && go run ./cmd/mvm snapshot create fb && go run ./cmd/mvm stop fb
go run ./cmd/mvm start fb --json 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print('boot_path=',d['boot_path'],'agent_ready=',d['agent_ready'])"
go run ./cmd/mvm exec fb -- echo degraded-ok
go run ./cmd/mvm delete fb; rm -rf ~/.mvm/vms/fb
```
Expected when restore is broken: `mvm start fb` succeeds (EXIT 0) with a `Warning: restore failed ...; falling back to cold boot` line, `boot_path= restore_fallback_cold`, `agent_ready= True`, and `degraded-ok` prints. Expected when restore works: `boot_path= snapshot_restore` and no warning (fallback never fires). Record the observed line in the findings file. If no applevz host, note "fallback compiles and is unit-guarded; behavioral check deferred — no hardware."

- [ ] **Step 6: Commit**

```bash
git add internal/cli/bootresult.go internal/cli/start.go internal/cli/bench.go
git commit -m "feat(applevz): degrade to cold-boot-from-checkpoint-disk when VZ restore fails; keep bench honest"
```

---

### Task 6: Full-suite verification & findings finalization

**Files:**
- Modify: `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md`

**Interfaces:** none — verification only.

- [ ] **Step 1: Full module build, vet, and test**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | tail -25
```
Expected: clean build, vet silent, every package `ok` (hardware-dependent tests may skip; no FAILs).

- [ ] **Step 2: Confirm the Swift helper still builds and signs**

```bash
make vz && codesign -d --entitlements :- bin/mvm-vz 2>&1 | grep -q virtualization && echo "VZ-OK"
```
Expected: `VZ-OK`. (Guards against any accidental change to `vz/` during diagnosis.)

- [ ] **Step 3: Finalize the findings file**

Ensure `docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md` has: a definitive `## VERDICT`, the Task-2 reproduction evidence, the Task-4 `### DECISION` block (if broken) or Task-3 benchmark (if fixed), and a one-paragraph `## Summary & next actions` closing the loop (e.g. "Restore still EPERMs on 25C56; OS_WIDE_VZ_BUG, FB1234567 filed; fallback shipped so start degrades to cold; revisit when macOS >26.2 lands" OR "Restore fixed on build X; README updated; fallback retained as safety net").

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/findings/2026-07-19-vz-restore-findings.md
git commit -m "diag(vz-restore): finalize findings and next actions"
```

---

## Out of Scope (explicitly)

- **A signing-based fix.** Both ad-hoc and Apple-Development signing were already tested (known-facts) and re-confirmed once in Task 4 Step 3; unless that re-confirmation flips (`MVM_SIGNING`), signing is a closed dead end, not a fix track here.
- **Building perpetual-standby.** This plan unblocks it (or documents why it stays blocked) but does not implement the warm-standby feature — that is a separate plan.
- **Firecracker-backend restore.** `runSnapshotRestore`'s daemon path (`internal/cli/snapshot.go:138`) is unaffected; this regression is Apple-VZ-only.
- **Changing the on-disk save format or the machine-identifier persistence** (`VMConfigBuilder.build` platform block, `internal/vm/applevz.go:143`) — only touched if Task 4's DECISION explicitly names `MVM_STATE_FILE` as the root cause.
- **Removing the checkpoint disk-snapshot rollback.** The fallback depends on it (start.go:285-293) to give cold-boot-from-checkpoint its filesystem state.
