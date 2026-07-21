# mvm Backlog

Prioritized by the product thesis: **mvm is a sandbox/microVM manager for
agent workloads**, adopting container ergonomics where they're free, staying
divergent where the sandbox thesis demands
(`docs/superpowers/specs/2026-07-19-image-vm-organization-design.md`).

Each workstream has an implementation plan in `docs/superpowers/plans/`.

## P0 — Core sandbox promises currently broken or weak

1. **VZ save/restore regression (macOS 26.2)** — `restoreMachineStateFrom`
   fails with `VZError 12 "permission denied"`; cold boot, checkpoint,
   pause/resume all still work. Blocks perpetual-standby and the 0.29s
   restore claim. Diagnosis dated 2026-06-26/27 (signing ruled out); needs
   re-test on current macOS first.
   Plan: `2026-07-19-vz-restore-diagnosis.md`
2. **Host-side net-policy enforcement** — policy is iptables *inside the
   guest*; a sandboxed agent with root in its VM can undo its own network
   restrictions. Move enforcement host-side (pf on macOS/applevz,
   Lima-side for Firecracker).
   Plan: `2026-07-19-host-side-net-policy.md`
3. **Volume mounts end-to-end** — `-V` accepted on both backends, believed
   non-functional end-to-end on both (mkdir-only on Firecracker; virtiofs
   tag/path semantics on applevz). Verify current truth, then finish.
   Plan: `2026-07-19-volumes-end-to-end.md`

## P1 — Backend parity (same sandbox features on both backends)

4. **`--startup`/`--secret` on the daemon/Firecracker path** — both error
   with "not yet supported" outside applevz (`internal/cli/start.go`).
5. **`--image` on applevz** — `runStartAppleVZ` takes no image parameter;
   custom images error on applevz hosts (guard in `mvm run`).
6. **`mvm logs` via daemon** — still reads files via `limaClient.Shell()`;
   last non-daemon path in the core lifecycle (`diff`/`doctor`/`template`
   are also Lima-direct but peripheral).
   Plan for 4–6: `2026-07-19-backend-parity.md`

## P2 — Hardening and polish

7. **`GetBackend()` error-returning variant** — currently defaults to
   `"firecracker"` on any store-load error; can silently bypass
   backend guards.
8. **Suppress `runStart` boot banner during foreground `mvm run`.**
9. **Test gaps** — `existingVMNames` daemon-merge branch; `runInspect`
   branch coverage.
10. **`-d` + auto-cleanup** (`--rm`-with-detach semantics) if ephemeral
    background sandboxes are wanted.
    Plan for 7–10: `2026-07-19-hardening-polish.md`

## Container ergonomics (active workstream)

11. **Ergonomics alignment** — `--env-file` and `-d` on `exec`,
    `[host-ip:]` prefix in `-p`, a `stats` command, `--rm` on `start`,
    `--format` on `inspect`/`list`.
    Plan: `2026-07-19-container-ergonomics.md`

## Deferred review findings (2026-07-21 whole-branch review of the four backend plans)

Minor findings from an independent 3-lens review; the Critical (port
`HostIP`/`Proto` command injection) and Important (image-name path traversal,
`-V` copy-in race, follow-mode error swallowing) findings were already fixed
(commits `84a4528`, `df70320`).

13. **Persisted-state trust in `RemovePortForwarding`** — `HostIP`/`Proto`
    read from a persisted `vm` (via `store.GetVM`) are interpolated into
    `sudo iptables` at stop/restore time with no re-validation on load. Post-fix
    the only writer of persisted `vm.Ports` is the now-validated
    `handleCreateVM`, so this only bites a state file written *before* the fix or
    tampered with host access — out of the remote-attacker model, but worth a
    `ValidatePort` sweep on load (or in `Remove/SetupPortForwarding`) for
    defense in depth.
14. **`mvm exec -it` skips secret injection on applevz** — the applevz
    interactive path returns before the secret-injection block, so `-it` execs
    get no secrets on applevz while the daemon path now injects them. Backend
    asymmetry, pre-existing; align them.
15. **`mvm run --rm --name X`** — `--rm` is a genuine no-op when `--name` is
    given, but the printed note ("already ephemeral … unless --name") is
    misleading in exactly that case. Docker removes a `--rm --name` container.
16. **Stats endpoint HTTP coverage** — `handleStatsVMs` and `Client.StatsVMs`
    have no functional (httptest) test; only `ParsePSOutput`/`filterStatsByName`
    unit tests and a schema-golden test exist. A route/content-type/backend-
    filter regression would pass every current test.
17. **Daemon-side log-read bounds** — `handleVMLogs` non-tail path does
    `io.ReadAll` on the whole `firecracker.log` into daemon memory (no
    server-side cap; the 64 MiB cap in `Client.StreamLogs` is client-side only),
    and follow mode holds a goroutine + ticker + open fd per connection.
18. **Image-endpoint error specificity** — `handleImageDownload`/`Delete`
    report 404 for *any* `os.Open`/`os.Stat` failure incl. permission errors,
    misdirecting operators diagnosing a filesystem problem.

## Conditional — build when a concrete need appears

12. **OCI image store + registry pull/push** (design-spec steps 3–4) —
    deferred until a real distribution need exists (fleet pulling a
    standard agent-sandbox image; base rootfs by digest). Requires its own
    brainstorm/design cycle first (OCI library choice, store migration);
    no plan is written until then.
