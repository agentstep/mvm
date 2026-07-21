# Container-dashboard → mvm compatibility matrix

**Goal:** let the existing `container-dashboard` app (a native macOS Zig app that
drives Apple's `container` CLI by spawning commands and parsing stdout) also
manage mvm, ideally by reusing its existing tested parsers.

**How the dashboard works** (ground truth, from `container-dashboard/src`): every
2s tick it spawns `container list --all --format json` + `container stats
--no-stream --format json`; on demand it runs `inspect`, `logs -f`, `exec`,
`image/volume/network list --format json`, `system status`, `system df --format
json`; actions are `stop`/`start`/`kill`/`delete --force` (+ `system stop`),
judged purely by exit code. Its parsers use `ignore_unknown_fields` (extra keys
are safe) and treat empty output / `[]` as a valid empty result. The **exact
camelCase key names, nesting, and units** below are mandatory — a renamed or
re-nested field silently reads as zero/empty.

## Legend

- **MATCH** — works today with at most an argv reshape in the adapter.
- **REFORMAT** — mvm has the data, but wrong shape / keys / units. Needs a
  container-compatible `--format` output mode.
- **MISSING** — mvm has no such command; needs new surface (a stub `[]` may be
  enough to render, full data is a feature).
- **SEMANTIC GAP** — the verb *means something different* in mvm; real model
  work, not formatting.

## Read commands

| Command | Container expects | mvm today | Verdict | Work |
|---|---|---|---|---|
| `list --all --format json` | JSON **array**; `configuration.{id, image.reference, resources.memoryInBytes, publishedPorts[]{hostPort,proto}}`, top-level `status`, `networks[0].ipv4Address` | `list --format json` → array of `VMResponse` (flat `name,status,guest_ip,pid,backend,ports[]{host_ip,host_port,guest_port,proto},created_at`) | **REFORMAT** | Re-nest under `configuration.*`; map `name→id`, `guest_ip→networks[0].ipv4Address`, port keys `host_port→hostPort`; **add `image` and `memoryInBytes`** (mvm list currently carries neither — image/memory live only in the spec/inspect). `status=="running"` literal already matches. |
| `stats --no-stream --format json` | flat array `{id, cpuUsageUsec (µs, **cumulative/monotonic**), memoryUsageBytes, memoryLimitBytes, numProcesses}` | `stats --format json` → array `VMStats{name, backend, pid, cpu_pct (**percent, instantaneous**), mem_mb (**MiB**), status}` | **REFORMAT + metric gap** | `name→id`, MiB→bytes. The hard part: dashboard wants **cumulative CPU microseconds** (it deltas across ticks), mvm emits an instantaneous `%cpu` from `ps -o %cpu`. Needs a different source (`ps -o time` / `/proc/<pid>/stat`). Also add `numProcesses`. |
| `inspect <id>` | JSON **array** (read `[0]`); nested `configuration.{id,image.reference,resources.{cpus,memoryInBytes},platform.{os,architecture},publishedPorts[]}`, `status`, `networks[0].ipv4Address`, `startedDate` (CoreFoundation epoch, read-not-shown) | `inspect <name>` (JSON is default) → single **object** `VMInspectResponse` (flattened `VMResponse` + nested `spec.{image,cpus,memory_mb,...}`) | **REFORMAT** | Wrap in a 1-element **array**; re-nest to `configuration.*`; add `platform.{os,architecture}` (mvm is always linux/arm64 — synthesize); `memory_mb→memoryInBytes`. `startedDate` is read-but-never-displayed → low priority. Dashboard strips `environment` from its raw-JSON pane, so mvm's env is safe to emit. |
| `image list --format json` | array `{reference, descriptor.{digest,size(bytes)}}` | `images list` → **plain text** `NAME SIZE(MiB)`; daemon has `ImageInfo{name,size_mb}` but CLI never emits JSON | **REFORMAT + add JSON mode** | Add `--format json`; `name→reference`, MiB→bytes for `descriptor.size`. `descriptor.digest` has **no mvm equivalent** (images are flat ext4 blobs, no content digest) — emit empty (dashboard tolerates) or synthesize a sha256 of the ext4. |
| `volume list --format json` | array `{name,format,driver,sizeInBytes}` | no `volume` noun | **MISSING** | Stub `mvm volume list --format json` → `[]` so the Volumes tab renders. Real population is the deferred volume-noun feature (BACKLOG). |
| `network list --format json` | array `{id,state,config.mode,status.ipv4Subnet}` | no `network` noun | **MISSING** | Stub `[]`, or emit one synthetic `"default"` network (mvm does have a fixed subnet). Full user-defined networks are deferred. |
| `system status` | **plain text**; scans for substrings `"is running"`, `"container-apiserver version: "`, `"application install root: "` | no `system` noun (closest: `doctor`, daemon `GET /health`) | **MISSING + semantic** | Add `mvm system status` emitting those load-bearing substrings. Semantic wrinkle: applevz has **no daemon at all**, so "is the daemon running" doesn't cleanly map on that backend. |
| `system df --format json` | single **object** `{containers,images,volumes}.{active,reclaimable(bytes),sizeInBytes(bytes),total}` (active/total are counts) | no `system` noun | **MISSING** | Add `mvm system df --format json` computing disk usage per resource type. VM rootfs + image cache are computable; volumes=0 until the volume noun exists. |
| `logs [-n N] -f <id>` | streaming plain-text lines; normal end ≠ failure; only spawn-failure is an error | `logs <name> [-f] [-n N] [--boot]` → streaming plain text to stdout | **MATCH** | Argv reshape only: `container logs -n N -f <id>` → `mvm logs -n N -f <name>` (cobra accepts flags in any position). Semantics ~equivalent (mvm default = guest `/var/log/messages`; needs the VM running). |
| `exec <id> <tokens...>` | `exec <id> tok0 tok1…` (no `-i/-t`), streaming, real exit code surfaced | `exec <name> -- tok0 tok1…` (requires `--`), streaming, propagates exit code | **MATCH** | Adapter inserts `--` after the name. Otherwise identical (streaming + exit code). |

## Action verbs (exit-code judged)

| Action | Container | mvm | Verdict |
|---|---|---|---|
| Stop | `stop <id>` | `stop <name>` (graceful) | **MATCH** — identical argv, exit-code based. |
| Delete | `delete --force <id>` | `delete --force <name>` (`--force` = stop-if-running then delete) | **MATCH** — identical argv; both remove a running VM. |
| Kill | `kill <id>` (SIGKILL) | no `kill` verb; SIGKILL = `stop --force` | **MATCH via adapter** — map `kill`→`stop --force`. |
| **Start** | `start <id>` — **boots an existing stopped container** | `start <name>` — **CREATES a new VM**; 409 `already exists` on a stopped name (Firecracker). applevz only reboots via a saved-state file, which a plain `stop` doesn't write. | **SEMANTIC GAP — the blocker.** |
| **Restart** | composed `stop` then `start` | inherits the Start gap | **SEMANTIC GAP** (depends on Start). |
| System stop | `system stop` (stops daemon) | `serve` manages the daemon; no clean stop verb, and applevz has no daemon | **MISSING + semantic.** |

## The one real blocker: start-a-stopped-VM

The dashboard's **Start** and **Restart** both call `container start <id>` to boot
a container that already exists in a stopped state. mvm's model treats a stopped
VM as **terminal-but-listed**: it stays in the store (keeps its name and net
allocation) but `mvm start <name>` can't reboot it — Firecracker returns 409
`already exists`, and applevz only reboots a name when a `state.vzvmsave`
snapshot exists (a plain `stop` doesn't create one). So "boot a previously
stopped VM in place" is a genuine mvm feature to build, not a formatting change.
It is the only item on this page that isn't either a MATCH or a bounded output
reformat.

## Where the adapter lives (recommendation)

Do the reformatting **mvm-side** (a container-compatible `--format json` mode),
so the dashboard reuses its existing parsers and 107 fixture tests unchanged. The
dashboard then needs only a thin backend selector that: swaps `container`→`mvm`,
inserts `--` before exec tokens, maps `kill`→`stop --force`, and — once mvm
supports it — routes Start/Restart to the new start-stopped-VM path.

## Suggested phasing

1. **Read views + safe actions** (no blocker touched): container-compat
   `--format json` for `list`/`stats`/`inspect`/`image list`; stub `volume
   list`/`network list` → `[]`; add `system status` (text) + `system df`
   (json); adapter argv mapping. Delivers Dashboard, Containers table, Images,
   Logs, Exec, Inspect, plus Stop/Kill/Delete against mvm. CPU can start
   best-effort.
2. **Start/Restart**: build start-a-stopped-VM in mvm (the blocker), then wire
   the two actions.
3. **Fidelity**: cumulative CPU microseconds, real volume/network population,
   image digests, accurate `system df`.

## Explicitly out of scope

OCI registry / `image pull|push|tag` — the dashboard only calls `image list`, so
none of the OCI distribution surface is on this path (stays deferred, BACKLOG
#12).
