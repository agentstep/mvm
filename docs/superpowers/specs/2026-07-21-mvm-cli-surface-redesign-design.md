# mvm CLI Surface Redesign — Design Spec

**Status:** Approved design (2026-07-21). Decomposes into staged implementation
plans (see §9).

## Goal

Redesign mvm's command surface using Apple's `container` CLI as the design
template, so mvm's *native* interface follows a proven, well-considered set of
conventions rather than an ad-hoc one. This is a native surface redesign, not a
compatibility shim: mvm genuinely adopts container's verb semantics, noun
grouping, flag names, and output conventions where they fit, and diverges
deliberately (and only) where mvm's microVM-sandbox model demands it.

A direct benefit — and a useful conformance test — is that tooling built for
Apple `container` (notably the `container-dashboard` app, which drives
`container` by spawning its CLI and parsing JSON) can drive mvm through mvm's own
native surface, because that surface now shares container's shapes.

## Why this fits (the models already match)

Apple's `container` and mvm are, independently, the same architecture: one
lightweight VM per workload, each with its own routable IP reachable directly
(port publishing is a convenience, not the only path in), explicit guest-kernel
management, and no single shared Linux VM. Adopting container's surface therefore
aligns mvm to a design that matches its substance — it is not bending mvm toward
a foreign model. Where container's surface reflects capabilities mvm does not yet
have (OCI registry, BuildKit builder), mvm simply doesn't adopt that part;
where mvm has capabilities container lacks (RAM-freeze pause/resume, snapshots,
security net-policy, the warm pool), mvm keeps them as honest, additive
divergence.

## Key architectural decoupling

mvm's CLI surface is a separate layer from its daemon HTTP API and the published
Go SDK (`sdk/`), which mirror the daemon's own request/response schema
(`VMResponse`, `PortMap`, etc.). This redesign reshapes **CLI verbs and CLI
output** only. The daemon HTTP API and SDK contract are **not** changed by this
spec — reducing blast radius and keeping the wire protocol stable. Where a new
verb needs new daemon behavior (e.g. start-a-stopped-VM), that is an additive
endpoint, not a change to existing ones.

## Global decisions

- **Breaking, done properly.** mvm is early (v0.2.x); existing verbs, flags, and
  JSON output are reshaped **in place** to match the target design. No
  backward-compat aliases or legacy output modes are carried. `scripts/
  integration-test.sh` and any local scripts are updated as part of the work.
- **CLI-layer only.** No change to the daemon HTTP API or `sdk/` schema.
- **Backend-aware, not backend-divergent.** Every command behaves the same from
  the user's view across the Firecracker (daemon) and applevz (no-daemon)
  backends; backend differences are hidden behind the command, never surfaced as
  different flags or output shapes.

---

## 1. Lifecycle verbs (top-level, unprefixed — as container exposes them)

| Verb | Semantics | Change vs today |
|---|---|---|
| `run <image> [cmd…]` | create + start, **foreground by default**; **persists by default** with an auto-generated adjective-noun name; `--rm` auto-deletes on exit; `-d/--detach` backgrounds; `-i/-t` for stdin/tty | **Reverses** today's ephemeral-by-default `run`. Agent one-shots use `mvm run --rm`. |
| `create <name-or-image>` | provision a VM (allocate config, prepare rootfs) and leave it **stopped**; accepts the same create-time flags as `run` | **New.** |
| `start <name>` | boot an **existing stopped** VM in place (preserves its rootfs/disk state; cold reboot, not RAM-resume); does not accept create-time config flags | **Changes** — today `start` creates a new VM. Enabled by the existing `firecracker.StartExisting` primitive (FC) and relaxing two guards in `runStartAppleVZ` (applevz). |
| `stop <name>` | graceful: `-s/--signal` (default TERM), `-t/--time` seconds (default 5) then kill; `-a/--all` | Add `-s`/`-t`/`-a`. VM stays listed as `stopped`. |
| `kill <name>` | send a signal immediately, `-s/--signal` (default KILL); `-a/--all` | **New verb** — today only reachable via `stop --force`. |
| `delete`/`rm <name>` | remove VM(s); `-f/--force` kills-then-removes a running VM; `-a/--all` | Matches today; keep `--force` = "remove even if running". |
| `list`/`ls` | **running-only by default**; `-a/--all` includes stopped; `--format json\|table` (default table); `-q/--quiet` (names only) | **Changes** — today lists all. |
| `exec <name> <cmd…>` | run a command in a running VM; **no `--` separator required**; `-i/-t`, `-d/--detach`, `-e/--env`, `--env-file`, `-w/--workdir`, `-u/--user` | **Changes** — today requires `--` before the command. |
| `logs <name>` | `--boot` (boot/console log), `-f/--follow`, `-n <N>` | Matches today. |
| `inspect <name>` | detailed JSON (native format, no `--format` needed) | Reshape JSON (§4). |
| `stats` | resource usage; `--no-stream` snapshot; `--format json\|table` | Reshape JSON + units (§4). |
| `build` | `-t/--tag`, `-f/--file`, `--size` | Align flag names. |

**No `restart` verb** — container has none; clients compose stop-then-start.

**No `paused` in the container vocabulary, but mvm keeps it** — mvm's `pause`/
`resume` (RAM freeze) are a superset (see §3); a paused VM appears with
`status: "running"`-family semantics or an explicit `paused` status in mvm's
richer state, surfaced honestly in `inspect`.

## 2. Noun groups (container structure)

- **`image`** (alias `i`): `ls`, `inspect`, `rm`, `prune`. Deliberately **omits**
  `pull`/`push`/`tag`/`save`/`load` — no OCI registry yet. Renames today's
  `images` → `image`; adds `inspect` (size, and digest when available) and
  `prune` (remove unused rootfs images).
- **`system`** (alias `s`): `status`, `df`, `version`, `logs`, `start`, `stop`.
  Absorbs today's `serve` (daemon lifecycle), `doctor` (folded into/behind
  `status`), and `version`. Backend-aware: on Firecracker, `status` reports the
  daemon; on applevz (no daemon) it reports backend readiness honestly
  ("applevz backend — no daemon required"). `df` computes disk usage across VMs,
  images, and volumes.
- **`volume`**: `create`, `ls`, `rm`, `inspect`, `prune` — **real named
  persistent volumes** (see §3 divergence note and §9 staging). Today only the
  `-v` flag exists.
- **`network`**: `ls`, `inspect` only (read-only) — reflects mvm's default
  network; user-defined networks are **not** created via this noun. `--net-policy`
  remains the real network control (see §3).

## 3. Deliberate divergences (mvm's reason to exist — kept, grouped container-style)

- **`pause` / `resume`** — RAM-freeze checkpoint/restore. No container analog.
  Kept as top-level verbs.
- **`snapshot`** (`create`/`restore`/`ls`) — disk+state snapshots. Kept as a noun.
- **`secret`** (`create`/`ls`/`rm`) — encrypted secrets injected at exec time
  (values never touch the daemon). Kept as a noun; a security primitive container
  lacks.
- **`pool`** (`status`/…) — warm-VM pool for instant starts. mvm-specific
  performance feature. Kept as a noun.
- **`idle`** — auto-pause after inactivity. Kept.
- **`--net-policy open|deny|allow:<domains>`** on `run`/`create` — sandbox
  egress control. No container analog; this, not a `network` noun, is mvm's real
  network-control surface. The `network` noun stays read-only (§2) precisely
  because mvm's model is per-VM policy, not user-defined bridges.
- **`volume` divergence note:** container volumes and mvm volumes will share the
  `volume` noun and CLI shape, but mvm's implementation is a persistent writable
  mount backed by the microVM model (building on the custom applevz virtio-fs
  kernel work), not container's storage driver. The CLI surface matches; the
  backing mechanism is mvm's own.
- **Retained utilities** (`ssh`, `install`, `diff`, `template`, `bench`,
  `preview`, `menu`, `forward-daemon`): kept. Reorganized under container-style
  grouping only where it clarifies; not renamed gratuitously.

## 4. Output conventions

- `--format json|table`, **default table** on `list`/`stats` (matching
  container); `inspect` is json-native (matching container).
- JSON shapes and units follow `docs/container-compat-matrix.md` exactly:
  container's nesting (`configuration.{id,image.reference,resources.
  memoryInBytes,publishedPorts[]}`, `networks[0].ipv4Address`, top-level
  `status`), flat `stats` (`{id, cpuUsageUsec, memoryUsageBytes,
  memoryLimitBytes, numProcesses}`), single-object `system df`. Units: **bytes**
  for memory/size, **cumulative microseconds** (monotonic) for CPU.
- `-q/--quiet` prints the identity column only.
- `inspect` MAY expose more than container (mvm's `spec`, net-policy, secrets
  *names*); extra keys are safe for container tooling (it ignores unknown
  fields). mvm never emits secret *values* or guest env values.

## 5. Flag alignment

Adopt container's flag names and short forms on the shared commands:
`-c/--cpus`, `-m/--memory`, `-v/--volume` (**was `-V`**), `-p/--publish
[host-ip:]host:guest[/proto]`, `-e/--env`, `--env-file`, `-d/--detach`,
`-i/--interactive`, `-t/--tty`, `--name`, `--rm`, `-w/--workdir`, `-u/--user`,
`-a/--all`, `-q/--quiet`, `-s/--signal`, `-t/--time`, `-f/--force`,
`-t/--tag`, `-f/--file`. mvm-only flags (`--net-policy`, `--seccomp`,
`--startup`, `--secret`) keep their names.

## 6. The three resolved forks

1. **`run` persists by default** (container semantics); `--rm` to auto-delete.
2. **`network` is read-only** (`ls`/`inspect`); `--net-policy` is the real control.
3. **`volume` real depth is staged** to a later implementation slice, not the first.

## 7. Error handling

- Verbs judged by exit code (0 = success) for action commands, matching
  container tooling expectations (the dashboard relies on this).
- Not-found → non-zero exit + stderr message; never a partial/ambiguous success.
- `list`/`stats`/`inspect` in `--format json` emit valid JSON (an empty array/
  object for the empty case, never `null` or empty stdout), so parsers treating
  empty-as-empty and parse-error-as-failure behave correctly.

## 8. Testing / conformance

- Unit tests per command for the reshaped output (golden JSON matching the
  container shapes in the compat matrix) and the new/changed verb semantics.
- **Conformance test:** the `container-dashboard` app driving mvm end-to-end is
  the acceptance signal for the lifecycle + output slice — its existing parsers
  (built for container) must consume mvm's native output unchanged.
- Existing suites (`internal/cli`, `internal/server`) updated for the breaking
  changes; `scripts/integration-test.sh` updated to the new surface.

## 9. Decomposition into implementation slices

Each slice becomes its own implementation plan (writing-plans), executed in
order. The design (this doc) defines the whole target surface; slices sequence
the build.

- **Slice 1 — Lifecycle + output + system (first).** `create`; `start`-resume
  (both backends); `kill`; `run` persist-by-default flip; `list` running-only +
  `-a`; `exec` without `--`; flag renames (`-V`→`-v`, add `-c`/`-m`); container-
  shaped JSON for `list`/`inspect`/`stats`; `image` noun (rename + `inspect`/
  `prune`); `system` noun (`status`/`df`/`version`/`logs`/`start`/`stop`).
  **Acceptance:** the dashboard drives mvm for Dashboard, Containers table,
  Images, Logs, Exec, Inspect, and Start/Stop/Kill/Delete/Restart.
- **Slice 2 — `volume` real depth.** Named persistent volumes: `volume
  create/ls/rm/inspect/prune`, a backing store outliving VMs, `-v name:/path`
  resolution, and a live writable mount (building on the applevz virtio-fs
  kernel). Populates the dashboard's Volumes tab with real data.
- **Slice 3 — Fidelity + network.** Cumulative-CPU-microsecond stats (replacing
  today's instantaneous `%cpu`); `network ls/inspect` reflecting the default
  network; image digests in `image inspect`.

Out of scope (tracked separately): OCI registry (`image pull/push/tag`), the
`builder`/`machine` groups, `--rosetta`/x86 emulation.

## 10. Migration notes

Breaking changes users will see: `run` no longer auto-deletes (use `--rm`);
`list` shows only running by default (use `-a`); `exec` drops the `--`; `-V`
becomes `-v`; `images` becomes `image`; `serve`/`doctor` move under `system`.
These are documented in the release notes for the version that ships Slice 1.
