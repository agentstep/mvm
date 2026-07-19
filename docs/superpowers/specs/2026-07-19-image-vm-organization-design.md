# Image/VM Organization: Long-Term CLI and Domain-Model Design

**Date:** 2026-07-19
**Status:** Approved direction, pending spec review
**Scope:** How mvm organizes its surface around the two nouns — images and VMs — and the decisions to lock in now for registry distribution, Docker-user onboarding, and ephemeral-run UX.

## Context

mvm is VM-first: `mvm start <name>` requires a VM name and takes `--image` as an
optional flag. Docker and Apple `container` are image-first: `run <image>`,
name optional. A survey of `container` 0.8.0's surface (2026-07-19) showed the
two CLIs converge on lifecycle verbs and flags (`logs -f/--boot/-n` are
flag-for-flag identical) but diverge on the primary noun and on the OCI
distribution layer, which mvm lacks entirely.

Motivations, all confirmed in scope: Docker-user onboarding, ephemeral run UX,
registry-distributed images, and long-term surface coherence.

## Goals

- Docker users can run `mvm run <image> -- cmd` and have it behave as expected.
- Images become shareable via standard OCI registries, eventually including the
  base rootfs itself.
- Existing consumers — Gateway's six commands, user scripts, SDKs — never break.
- The sandbox-differentiator commands (pause, snapshot, pool, secret,
  net-policy, preview, install) remain first-class and are never eroded by
  Docker-compat pressure.

## Non-Goals

- A big-bang rename or pivot away from `mvm start <name>`.
- Full Docker CLI compatibility. Compatibility is adopted only where it is
  cheap and inherits an ecosystem.
- Container-to-container network topology (named networks à la
  `container network`). mvm networking stays policy-shaped (`--net-policy`).

## North Star

The daemon HTTP API is the product; the CLI is a thin client. The domain model
(image, VM spec, VM state) is designed at the API layer and versioned (`/v1`).
CLI JSON output is a stable interface with additive-only evolution. The
end-state CLI is a two-noun surface (`vm`, `image`) with Docker-shaped verbs
and the current top-level commands preserved permanently as shortcuts.

## The Five Load-Bearing Decisions

These are the hard-to-reverse commitments, locked in now:

### 1. Image store is OCI layout from day one of the image work

Images are stored as OCI artifacts (manifest + layers, digest-addressed). The
bootable ext4 rootfs is a **derived cache keyed by digest**, not the source of
truth. This mirrors Apple containerization's model (OCI image → block image)
and makes pull/push/tag/dedup free later instead of retrofits. `mvm build`
output migrates from ad-hoc named rootfs blobs to this store.

### 2. Reference grammar is Docker's, never bespoke

`[registry/]repo[:tag][@digest]`, with mvm defaulting the registry component.
The base rootfs becomes an image ref pinned by digest per release and pulled by
`mvm init`. The default image is configuration (cf. `container`'s `image.init`
property), not a special case in code.

### 3. VMs reference images by digest, not tag

A VM's stored spec records the resolved digest at create time. Tags are
mutable pointers; instances must be reproducible and inspectable.

### 4. The VM spec is a first-class declarative record

The create request (image digest, cpus, memory, ports, mounts, net-policy,
secret refs, startup recipe) is persisted as the VM's spec and returned by a
new `mvm inspect`. Consequences: a template *is* a named spec; a future
declarative file (`mvm.yaml`) is a serialization question, not a redesign;
Gateway/SDKs get a stable object.

### 5. Verb semantics: `run` is image-first ephemeral; `start` is upsert, forever

- `mvm run <image> [-- cmd]`: auto-generated name, `--rm` semantics by
  default, `--name` to opt into durability, `-d` to detach.
- `mvm start <name>`: documented as an intentional idempotent
  create-or-boot primitive (k8s-apply-flavored). It will **not** migrate to
  Docker's boot-existing-only semantics — that is the one breaking change on
  the table and it is explicitly rejected. Divergence chosen deliberately and
  documented beats divergence by accident.

## Compatibility Policy

Adopt where free (inherits an ecosystem): OCI image format, registry
protocol, reference grammar, Dockerfile-subset builds, familiar flags
(`--rm`, `-d`, `--env-file` on exec, `[host-ip:]` prefix in `-p`).

Diverge where it is the thesis: pause/resume, snapshot/diff, warm pool,
secrets, net-policy, preview, install, templates. These are why mvm exists.

## Migration Path

Each step is additive and shippable alone; each gets its own spec → plan
cycle. Steps 1–2 need no image-format work; step 3 needs no registry.

1. **Spec + inspect.** Daemon persists the VM spec; add `mvm inspect`.
   Foundation only, no behavior change.
2. **`mvm run <image>`.** Thin CLI wrapper over the existing daemon create
   path: auto-naming, `--rm`, `--name`, `-d`. Delivers onboarding + ephemeral
   UX.
3. **OCI-ify the image store.** `mvm build` writes OCI layout; rootfs becomes
   a derived cache; `mvm images` grows `inspect` and `tag`.
4. **Distribution.** `mvm images pull/push`, registry auth
   (`registry login/logout` analog), base image delivered as a pulled ref.
5. **Surface consolidation.** `vm`/`image` nouns with old top-level commands
   as permanent aliases. Cosmetic, last, optional.

## Guardrails

- **Deprecation policy:** nothing removed without deprecation warnings across
  N (≥2) minor releases. Policy written down in CONTRIBUTING.md.
- **Gateway compat test:** an automated test asserting the six Gateway
  commands (`version`, `pool status`, `start --net-policy deny`, `exec`,
  `delete --force`, `list --json`) never change observable behavior.
- **Golden JSON tests:** schemas for `--json` output are tested; evolution is
  additive-only.
- **API versioning:** daemon routes under `/v1` before step 1 lands.

## Testing

- Step-level testing is defined per-step in each implementation spec.
- Cross-cutting: the Gateway compat suite and golden JSON tests run in CI from
  step 1 onward.

## Success Criteria

- A Docker user's first session (`mvm run <image> -- cmd`, `mvm ls`,
  `logs`, `rm`) works on muscle memory without reading docs.
- An image built on one machine is `push`ed and `run` on another with no
  side-channel file copying.
- Gateway integration passes its compat suite unchanged through all five
  steps.
- No existing command or flag is removed or repurposed.

## Open Questions (deferred to per-step specs)

- Auto-name scheme for `run` (docker-style adjective-noun vs short hash).
- Whether `run` without an image arg errors or uses the configured default
  image (leaning: require the arg; `mvm run base` is the documented default).
- Snapshot/template relationship to the image store (are snapshots image-like
  artifacts?). Out of scope until step 3.
