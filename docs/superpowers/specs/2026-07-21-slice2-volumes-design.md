# Slice 2 — Named Volumes Design (DEFERRED, drafted 2026-07-21)

**Status:** designed + fully drafted, execution **deferred** (Slice 3 done first).
Pick this up when real persistent volumes are wanted. Slice 3 (network noun, FC
cumulative-CPU endpoint, image digests) shipped ahead of it.

## Decided architecture (do not re-litigate)

A **named volume = an ext4 image file attached as a virtio-block device** — the
Apple-`container` model (`volume.img`). Distinct from a **bind mount**
(`-v /host/path:/guest`, unchanged: FC tar copy-in / applevz virtio-fs). Chosen
because it is a TRUE persistent block volume on *both* backends, maps exactly to
the dashboard schema (`{name, format:"ext4", driver:"local", sizeInBytes}`), and
**needs no custom kernel** (virtio-block is stock — sidesteps the applevz
virtio-fs kernel gap entirely).

- **`-v <src>:<guest>` resolution:** `<src>` with no `/` (and not `.`/`..`) → a
  named-volume reference (looked up in the store, error if absent); `<src>` with
  a `/` → bind mount (existing path, untouched).
- **Firecracker:** image at `firecracker.VolumesDir()/<name>.ext4` (daemon-side,
  `/opt/mvm/volumes`), created + `mkfs.ext4`'d by the daemon at `volume create`;
  attached as a second non-root entry in `fcConfig.Drives`; guest mounts
  `/dev/vdb` (etc.) post-boot (no in-guest mkfs — daemon formatted it).
- **applevz:** image at `~/.mvm/volumes/<name>.ext4`, created mac-side as a raw
  sparse file (`O_EXCL`+`Truncate` — macOS has no `mkfs.ext4`); attached via
  `VZDiskImageStorageDeviceAttachment` after the rootfs; guest
  **formats-on-first-mount** (`blkid || mkfs.ext4`) then mounts.
- **State:** new `state.Volume{Name,Driver,Format,SizeInBytes,CreatedAt}` +
  `State.Volumes map[string]*Volume` (nil-guarded, mirrors `VMs`) + CRUD.
- **CLI noun `volume`:** create/ls/rm/inspect/prune; `ls`/`inspect --format json`
  → `cfVolume` (containerfmt.go). Additive daemon endpoints `POST/GET/DELETE
  /volumes`. `CreateVMRequest` gains `NamedVolumes []NamedVolumeMount` (additive
  omitempty — SDK unaffected).

## Reconciliation required before executing the drafted tasks

The plan was drafted by three parallel agents (management / FC / applevz) whose
seams conflict — unify these first:

1. **One `classifyVolumeSpec` signature:** standardize on
   `func classifyVolumeSpec(spec string) (named bool, ref, guestPath string, err error)`
   (parses a full `-v` entry). Group A drafted a `(src) bool` variant and Group C
   a struct-returning variant — replace both with this. Fix `unreferencedVolumes`
   (management) to consume it.
2. **One named-volume threading path through BOTH backends:** classify once in
   the `run`/`create`/`start` RunE via `splitVolumeSpecs(store, rawSpecs) →
   (binds []string, named []server.NamedVolumeMount)`; thread `named` through
   `runStart` to *both* `runStartViaDaemon` (→ `CreateVMRequest.NamedVolumes`)
   AND `runStartAppleVZ` (→ resolve each `named.Name` to
   `applevzVolumeImagePath`, attach as `--disk`, format+mount in-guest). The FC
   draft (VB4) erroneously *errors* on applevz named volumes while the applevz
   draft (VC4) implements them via its own in-function classifier — delete VC4's
   `applevzClassifyVolumes` and the VB4 applevz guard; use the single
   pre-classified `named` slice. Only binds go to `parseVolumes` (so named refs
   are never absolutized — resolves Group C's ordering concern).
3. **Declare `CreateVMRequest.NamedVolumes` + `NamedVolumeMount` exactly ONCE**
   (in the FC create-wiring task, which uses them), not also in the volume-
   endpoints task.
4. **`run`/`create` must also `splitVolumeSpecs`** (not pass `nil` named) so
   `mvm run -v myvol:/data` doesn't silently absolutize a named ref into a bind.

## Known deferrals inside Slice 2 (noted in the drafts)

- Resume path (`handleStartVM`/`StartExisting`) does not re-attach named volumes
  — `state.VMSpec` isn't extended with a named-volume list; a cold-reboot resume
  boots rootfs-only. Follow-up: persist `NamedVolumes` on the spec and thread
  into `StartExisting`.
- A single block-device volume is single-attach (no concurrent sharing across
  running VMs); volume resize is out of scope.

## Verification reality

Unit-testable at the string/struct level throughout (drive-array builder, mount-
command builder, image lifecycle, endpoints, classification). The actual
block-device boot/mount (real `mkfs.ext4` in Lima, FC second drive, Swift VZ
disk attach, in-guest format-on-first-mount) requires a **live daemon/VM smoke
test** — not runnable in the dev environment where this was designed.
