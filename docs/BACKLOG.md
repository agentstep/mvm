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

## Conditional — build when a concrete need appears

12. **OCI image store + registry pull/push** (design-spec steps 3–4) —
    deferred until a real distribution need exists (fleet pulling a
    standard agent-sandbox image; base rootfs by digest). Requires its own
    brainstorm/design cycle first (OCI library choice, store migration);
    no plan is written until then.
