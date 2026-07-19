# Volume Mounts End-to-End Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **Task 1 is investigation-only and must run first — Tasks 4-6 branch on its findings; do not skip its decision points.**

**Goal:** Make `-V hostPath:guestPath` (on both `mvm start` and `mvm run`) actually work end-to-end: on **applevz**, a true live, bidirectional virtiofs mount (host and guest see each other's writes immediately); on **Firecracker**, a one-shot copy-in of the host directory into the guest at boot (host→guest only, not live) — a deliberate, documented trade-off because Firecracker's VMM has never implemented a virtio-fs (or 9p) device (see Global Constraints). `mvm run base -V ./src:/app -- make test` must work on both backends under these documented semantics.

**Architecture:**
- **applevz**: Apple's `Virtualization.framework` already exposes `VZVirtioFileSystemDeviceConfiguration`/`VZSharedDirectory` — a real virtio-fs *host* device — via the `mvm-vz` Swift helper (`vz/Sources/mvm-vz/VM/VMManager.swift:106-117`). Today it's wired with a broken tag (the guest path is misused as the virtio-fs tag, `vz/Sources/mvm-vz/Commands/Create.swift:53-66`), and **nothing on the guest side ever mounts it** — no `mount -t virtiofs` call exists anywhere in `internal/cli/start.go`, the guest agent (`agent/`), or the rootfs init script (`internal/firecracker/scripts/chroot-setup.sh`). This plan fixes the tag (Task 5) and adds the missing guest-side mount, driven from `internal/cli/start.go`'s existing post-boot agent-setup sequence (Task 6) — the same place port-forwarding and network-policy setup already happen for applevz.
- **Firecracker**: Firecracker's VMM does not implement virtio-fs or 9p (a long-standing, deliberate upstream limitation — Firecracker's device model is net/block/vsock/balloon only). `internal/firecracker/security.go:24-41`'s `SetupVolumeMounts` currently only `mkdir -p`s the guest path and copies nothing. This plan replaces it with a real one-shot copy-in: tar the host directory, base64-encode it, and ship it over the same `agentclient.Client.Exec` primitive the daemon already uses for `mvm exec` on Firecracker (`internal/server/routes.go:322-324`, `internal/agentclient/client.go:99-119`), extracting it into the guest path with `tar -x`. This is bounded by the agent protocol's existing 10 MiB frame cap (`agent/internal/protocol/frame.go:125`, `internal/agentclient/protocol.go:40`), so it is scoped to small directories (source trees, config), not arbitrary bind-mount workloads — exactly the gap applevz's live share exists to cover.
- Both backends already run the *same* prebuilt kernel (`~/.mvm/cache/vmlinux`, Firecracker's own CI kernel for arm64 — see `internal/firecracker/install.go:68-107`, downloaded from `spec.ccfc.min.s3.amazonaws.com/firecracker-ci/v1.13/...`) and the *same* Debian rootfs (`internal/firecracker/scripts/build-rootfs.sh`). Whether that kernel has `CONFIG_VIRTIO_FS`/`CONFIG_FUSE_FS` built in is unverified and gates the applevz work — see Task 1, Step 3.

**Tech Stack:** Go 1.26 (`go.mod`), Swift (SwiftPM, `vz/Package.swift`), stdlib-only Go tests (no testify) matching `internal/cli` / `internal/firecracker` conventions. Debian Bookworm arm64 guest rootfs.

## Global Constraints

- **Firecracker cannot get a live/bidirectional mount — this is not a bug to fix, it's an upstream VMM limitation.** Do not attempt virtio-blk-loopback-image or NFS-over-TAP schemes in this plan: virtio-blk-with-a-constructed-image can't be live (no filesystem-level sync back to a directory), and NFS-over-TAP would require standing up and hardening an NFS server inside the Lima VM (or bare Linux host) for every `mvm start -V`, an operational cost and attack-surface increase disproportionate to what's actually needed here. The copy-in approach in Task 3 reuses existing, already-tested primitives (`agentclient.Client.Exec`) and ships in one task instead of a new service.
- **Firecracker copy-in is one-shot, at boot, and capped at ~6 MiB of tar data.** No copy-out, no live sync, no `mvm cp`. Host-side edits made after boot do not appear in the guest; guest-side edits do not appear on the host, ever (not even at delete). This must be stated in `--help` text and doc comments, not just this plan.
- **`-V` host paths must resolve to something the process that actually performs the copy/mount can see.** On macOS, the Firecracker daemon runs *inside* the Lima VM (see `internal/firecracker/executor.go:14-20`'s `Executor` doc comment); Lima's own config already mounts the user's `$HOME` into the Lima VM at the identical path (`internal/lima/lima.go:168`: `.mounts=[{"location":"~","writable":true}]`). A host path outside `$HOME` is invisible inside Lima and must fail with a clear error, not a silent empty copy. applevz has no such constraint (`mvm-vz` runs directly on macOS), but both backends need an *absolute* host path — the Swift side's `URL(fileURLWithPath:)` (`vz/Sources/mvm-vz/VM/VMManager.swift:109`) resolves relative paths against the `mvm-vz` process's own cwd, not the user's, which is almost never what's wanted.
- **Do not change `SetupVolumeMounts`'s call site behavior for zero volumes.** `internal/server/routes.go:262-266` only calls it when `len(req.Volumes) > 0`; keep that guard.
- **Remote/cloud daemon hosts are out of scope for host-path resolution.** The recent "cloud install + remote CLI" work (commit `c0cdd21`) means the CLI and daemon can run on different machines; resolving what "the host path" means when the CLI is local and the daemon is a remote KVM box is a separate, larger problem this plan does not solve. `parseVolumes` (Task 2) absolutizes against the *CLI process's* cwd, which is correct for the local-Lima and applevz cases this plan targets and explicitly wrong for a remote daemon — call this out in the flag's `--help` text, don't silently pretend it works.
- Match existing code style: tabs, stdlib-only tests, `go vet` clean. Repo module path is `github.com/agentstep/mvm`. Run all Go commands from `/Users/paulmeller/Projects/firecracker`. Swift changes are built via `cd vz && swift build -c release` and the resulting binary is what `internal/vzhelper.HelperBinary()` resolves to — confirm the exact invocation with `grep -rn HelperBinary internal/vzhelper/` before Task 5's build step if the binary isn't where expected.
- **TDD for all Go changes.** Every task that adds Go logic follows write-failing-test → confirm fail → implement → confirm pass. The Swift change (Task 5) has no local Swift test harness in this repo (`vz/` has no `Tests/` directory — confirmed via `find vz/Sources -iname '*Tests*'` returning nothing beyond the vendored `swift-argument-parser` checkout); it is verified via Task 1's manual boot-and-inspect commands instead, called out explicitly in Task 5.

---

### Task 1: Verification — establish ground truth before changing anything

**Files:** none (investigation only — the findings below are already grounded by reading `internal/firecracker/security.go`, `internal/vm/applevz.go`, `vz/Sources/mvm-vz/Commands/Create.swift`, `vz/Sources/mvm-vz/VM/VMManager.swift`, and `internal/cli/start.go`; this task re-confirms them against a real running system and, critically, resolves the one thing that can't be determined from source: whether the shared prebuilt kernel has virtio-fs support).

**Interfaces:** none.

- [ ] **Step 1: Confirm current Firecracker behavior (mkdir-only, no copy)**

Run:
```bash
mkdir -p /tmp/vol-verify-src && echo "hello-from-host" > /tmp/vol-verify-src/marker.txt
mvm start fc-vol-test -V /tmp/vol-verify-src:/data
mvm exec fc-vol-test -- ls -la /data
mvm exec fc-vol-test -- cat /data/marker.txt
mvm delete fc-vol-test
```
Expected (from reading `internal/firecracker/security.go:26-41` — `SetupVolumeMounts` only runs `mkdir -p %s`): `ls -la /data` shows an empty directory (just `.` and `..`); `cat /data/marker.txt` fails with `No such file or directory`. If this instead shows the file, the mkdir-only description is stale — stop and re-read `internal/server/routes.go:262-266` for what actually runs before proceeding to Task 3.

- [ ] **Step 2: Confirm current applevz behavior (nothing mounts at all)**

Run (only if this machine has Apple Silicon + applevz configured — check with `mvm doctor`):
```bash
mvm start vz-vol-test -V /tmp/vol-verify-src:/data
mvm exec vz-vol-test -- ls -la /data
mvm delete vz-vol-test
```
Expected (from reading `internal/cli/start.go`'s `runStartAppleVZ`, lines 221-450ish — there is no volume-mount step anywhere in the post-boot agent sequence, only DNS/net-policy/port-forwarding/startup): `ls -la /data` fails with `No such file or directory` — `/data` was never created (unlike Firecracker, nothing even mkdirs it) because the guest never learns about the share at all today.

- [ ] **Step 3: THE GATING CHECK — does the shared kernel have virtio-fs support?**

Run:
```bash
mvm start kernel-check -d
mvm exec kernel-check -- sh -c 'mkdir -p /mnt/vftest && mount -t virtiofs anytag /mnt/vftest 2>&1; echo "EXIT:$?"'
mvm delete kernel-check
```
This works identically on either backend since both boot the same `~/.mvm/cache/vmlinux` (`internal/cli/start.go:235` and `internal/firecracker/install.go`'s `DownloadImages`) — use whichever backend `mvm doctor` reports as configured.

Two possible outcomes:
- **`mount: mounting anytag on /mnt/vftest failed: No such file or directory` or `unknown filesystem type 'virtiofs'`** → the kernel has no virtio-fs driver. This is the expected outcome: the kernel is Firecracker's own CI kernel build (`internal/firecracker/scripts/build-rootfs.sh:15-22`, pulled from `firecracker-ci/v1.13/${ARCH}/vmlinux-6.1.*`), configured for exactly Firecracker's device set (virtio-net/blk/vsock/balloon) — Firecracker has never had a virtio-fs device, so its CI kernel config has no reason to carry `CONFIG_VIRTIO_FS`/`CONFIG_FUSE_FS`. **If this is the outcome: Task 4 (custom applevz kernel) is required before Task 5/6 can do anything — do not skip it.**
- **`mount: mounting anytag on /mnt/vftest failed: Invalid argument` (or similar — the mount syscall reaches the virtiofs driver but the device/tag isn't attached because no `--share` was passed for this VM)** → virtio-fs support is already present in the kernel. **Skip Task 4 entirely** and go straight to Task 5.

Record the actual output in the task tracker before proceeding — Tasks 4-6 below are written to branch on it explicitly.

- [ ] **Step 4: Confirm the Swift-side tag bug independently of Step 3**

This is a code-reading confirmation, not a live check (the tag bug and the kernel-support question are independent failures — fixing the tag doesn't help if the kernel can't mount virtiofs at all, and vice versa). Read `vz/Sources/mvm-vz/Commands/Create.swift:53-66`: the `--share` option's `NOTE` comment says `parts[0]=hostPath, parts[1]=tag`, but the value actually placed at `parts[1]` is the *guest path* from `-V hostPath:guestPath` (see `internal/vm/applevz.go:147-150`, which passes the volume string through unmodified as `--share vol`). So today, a `-V ./src:/app` share gets tag `/app` — a path, not a short opaque identifier — handed straight to `VZVirtioFileSystemDeviceConfiguration(tag:)` (`VMManager.swift:111`). Confirm whether this actually throws during `try vzConfig.validate()` (`Create.swift:86`) by running:
```bash
mvm start tag-check -V /tmp/vol-verify-src:/deeply/nested/app 2>&1
cat ~/.mvm/vms/tag-check/mvm-vz-stderr.log 2>/dev/null
mvm delete tag-check 2>/dev/null || true
```
Either it throws (confirms the tag must never be a raw path) or it silently "succeeds" with an unusable tag (confirms the guest-side mount, which needs to reference the same tag, has no way to know what it is). Both outcomes point to the same fix in Task 5 — this step is about confirming there's no *third* explanation you're missing before writing code.

---

### Task 2: `parseVolumes` — CLI-layer format validation and host-path absolutization

**Files:**
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/start_test.go`

**Interfaces:**
- Produces: `func parseVolumes(volumes []string) ([]string, error)`. Task 3 and Task 6 assume volumes reaching `SetupVolumeMounts`/`virtiofsMountCommands` are already in this validated, absolutized form. `newStartCmd`'s and `newRunCmd`'s `RunE` closures call it exactly like `parsePorts`.
- Consumes: nothing new (stdlib `path/filepath`, `strings`, `os`).

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/start_test.go` (mirrors `TestParsePorts` at line 10):

```go
// === parseVolumes ===

func TestParseVolumes(t *testing.T) {
	cwd, _ := os.Getwd()

	tests := []struct {
		name    string
		input   []string
		wantErr bool
		check   func(t *testing.T, got []string)
	}{
		{
			name:  "absolute host path passes through",
			input: []string{"/tmp/src:/app"},
			check: func(t *testing.T, got []string) {
				if len(got) != 1 || got[0] != "/tmp/src:/app" {
					t.Errorf("got %v, want [/tmp/src:/app]", got)
				}
			},
		},
		{
			name:  "relative host path resolves against cwd",
			input: []string{"./src:/app"},
			check: func(t *testing.T, got []string) {
				want := filepath.Join(cwd, "src") + ":/app"
				if len(got) != 1 || got[0] != want {
					t.Errorf("got %v, want [%s]", got, want)
				}
			},
		},
		{name: "missing colon", input: []string{"/tmp/src"}, wantErr: true},
		{name: "empty host path", input: []string{":/app"}, wantErr: true},
		{name: "relative guest path rejected", input: []string{"/tmp/src:app"}, wantErr: true},
		{name: "nil input ok", input: nil},
		{name: "empty slice ok", input: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVolumes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseVolumes(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}
```

Add `"os"` and `"path/filepath"` to the import block if not already present (check first — `start_test.go`'s current imports are `encoding/json`, `testing`, `github.com/agentstep/mvm/internal/state`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestParseVolumes -v`
Expected: FAIL — compile error `undefined: parseVolumes`.

- [ ] **Step 3: Write minimal implementation**

Insert into `internal/cli/start.go` immediately after `parsePorts` (after line 108, before `func runStart` at line 110):

```go
// parseVolumes validates each "hostPath:guestPath" entry and resolves a
// relative hostPath to an absolute one against the CLI process's own cwd.
//
// Absolutizing here (not deeper in the stack) matters because both backends
// eventually need an unambiguous path: on Firecracker, the daemon that reads
// hostPath runs inside the Lima VM, which only sees paths under $HOME (Lima's
// own mount config — see internal/lima/lima.go's ".mounts" setting); on
// applevz, the Swift helper's VZSharedDirectory resolves a relative hostPath
// against its own process cwd, not the user's, which is never what's wanted.
// This does NOT resolve the "remote daemon" case (CLI and daemon on
// different machines) — see the plan's Global Constraints.
//
// guestPath must be absolute: it becomes a mount target (applevz) or a tar
// extraction directory (Firecracker), and a relative guest path is ambiguous
// once the guest's cwd for that operation isn't guaranteed (root's home
// varies; no shell profile has run yet at mount time).
func parseVolumes(volumes []string) ([]string, error) {
	var result []string
	for _, v := range volumes {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid volume format %q (expected hostPath:guestPath)", v)
		}
		hostPath, guestPath := parts[0], parts[1]
		if !filepath.IsAbs(guestPath) {
			return nil, fmt.Errorf("invalid volume %q: guest path %q must be absolute", v, guestPath)
		}
		if !filepath.IsAbs(hostPath) {
			abs, err := filepath.Abs(hostPath)
			if err != nil {
				return nil, fmt.Errorf("resolve host path %q: %w", hostPath, err)
			}
			hostPath = abs
		}
		result = append(result, hostPath+":"+guestPath)
	}
	return result, nil
}
```

Add `"path/filepath"` to `start.go`'s import block if not already present (it already imports `"os"` and `"path/filepath"` per the current file header at lines 11-12 — confirm before editing, don't duplicate the import).

Wire it into both call sites:

In `internal/cli/start.go`'s `newStartCmd` `RunE` (currently lines 53-57), after the existing `portMaps, err := parsePorts(ports)` block:
```go
			volumes, err = parseVolumes(volumes)
			if err != nil {
				return err
			}
```

In `internal/cli/run.go`'s `newRunCmd` `RunE` (lines 121-129), after `portMaps, err := parsePorts(ports)`:
```go
			volumes, err = parseVolumes(volumes)
			if err != nil {
				return err
			}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestParseVolumes|TestParsePorts' -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go internal/cli/run.go
git commit -m "feat(cli): validate and absolutize -V host paths before they reach either backend"
```

---

### Task 3: Firecracker — real copy-in for `SetupVolumeMounts`

**Files:**
- Modify: `internal/firecracker/security.go`
- Modify: `internal/firecracker/network_test.go`
- Modify: `internal/server/routes.go` (drop the now-unused `Executor` argument at the call site)

**Interfaces:**
- Consumes: `agentclient.New`, `agentclient.FirecrackerVsockDialer` (`internal/agentclient/client.go:36`, `internal/agentclient/dial.go:45-55`, already used identically at `internal/firecracker/pool.go:351` and `internal/server/routes.go:322-324`), `VsockUDSPath` (`internal/firecracker/config.go:43-45`), `shellQuoteForSSH` (`internal/firecracker/security.go:43-45`, already in this file).
- Produces: `func SetupVolumeMounts(vm *state.VM, volumes []string) error` (signature change: drops the unused `ex Executor` parameter — the new implementation talks to the guest directly over vsock, not through the Lima-host `Executor`). `internal/server/routes.go:263` is the only caller; update it.
- Produces (internal, tested directly): `func buildTarArchive(hostDir string) ([]byte, error)`.

- [ ] **Step 1: Write the failing tests**

Replace the existing volume tests in `internal/firecracker/network_test.go`. The current `TestSetupVolumeMountsFormatValidation` (lines 98-113) is a no-op — it builds local slices and never calls the function under test. Replace it and update the two real tests (`TestSetupVolumeMountsInvalidFormat` at 209-217, `TestSetupVolumeMountsEmptyList` at 219-232) to match the new signature:

```go
// === buildTarArchive ===

func TestBuildTarArchiveIncludesNestedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("top-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("nested-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := buildTarArchive(dir)
	if err != nil {
		t.Fatalf("buildTarArchive: %v", err)
	}

	found := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, _ := io.ReadAll(tr)
		found[hdr.Name] = string(content)
	}

	if found["top.txt"] != "top-content" {
		t.Errorf("top.txt = %q, want top-content", found["top.txt"])
	}
	if found[filepath.Join("sub", "nested.txt")] != "nested-content" {
		t.Errorf("sub/nested.txt = %q, want nested-content", found[filepath.Join("sub", "nested.txt")])
	}
}

func TestBuildTarArchiveMissingDir(t *testing.T) {
	_, err := buildTarArchive(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Error("buildTarArchive on a missing dir should error")
	}
}

func TestBuildTarArchiveTooLarge(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxVolumeCopyBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := buildTarArchive(dir)
	if err == nil {
		t.Error("buildTarArchive over the size cap should error")
	}
}

// === SetupVolumeMounts: format validation still happens before any I/O ===

func TestSetupVolumeMountsInvalidFormat(t *testing.T) {
	vm := &state.VM{Name: "doesnotexist"}
	err := SetupVolumeMounts(vm, []string{"/just/one/path"})
	if err == nil {
		t.Error("should error on invalid volume format (missing colon)")
	}
}

func TestSetupVolumeMountsEmptyList(t *testing.T) {
	vm := &state.VM{Name: "doesnotexist"}
	if err := SetupVolumeMounts(vm, nil); err != nil {
		t.Errorf("empty volume list should not error: %v", err)
	}
	if err := SetupVolumeMounts(vm, []string{}); err != nil {
		t.Errorf("empty volume slice should not error: %v", err)
	}
}

func TestSetupVolumeMountsMissingHostDir(t *testing.T) {
	vm := &state.VM{Name: "doesnotexist"}
	err := SetupVolumeMounts(vm, []string{"/definitely/does/not/exist:/data"})
	if err == nil {
		t.Error("should error when the host directory doesn't exist, before ever dialing the guest")
	}
}
```

Delete the old no-op `TestSetupVolumeMountsFormatValidation` (lines 98-113) entirely — it tests nothing and its replacement above (`TestBuildTarArchive*`) covers the real logic. Delete the old `mockTestExecutor` type (lines 234-239, currently only used by the two tests above) if nothing else in the file still uses it — check with `grep -n mockTestExecutor internal/firecracker/network_test.go` first.

Add `"archive/tar"`, `"bytes"`, `"io"`, `"os"`, `"path/filepath"` to `network_test.go`'s import block as needed (check what's already imported before adding — the file already imports `"strings"`, `"testing"`, `"github.com/agentstep/mvm/internal/state"` per the surrounding tests).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firecracker/ -run 'TestBuildTarArchive|TestSetupVolumeMounts' -v`
Expected: FAIL — compile errors (`undefined: buildTarArchive`, `undefined: maxVolumeCopyBytes`, `SetupVolumeMounts` called with wrong argument count once the old two-arg call sites in the new tests don't match the still-three-arg production signature).

- [ ] **Step 3: Write minimal implementation**

Replace `internal/firecracker/security.go` in full:

```go
package firecracker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/state"
)

// Seccomp profiles — restrict syscalls inside the guest.
var seccompProfiles = map[string]string{
	"strict": `iptables -A OUTPUT -p tcp --dport 80 -j DROP
iptables -A OUTPUT -p tcp --dport 443 -j DROP
chmod 000 /usr/bin/wget /usr/bin/curl 2>/dev/null || true
chmod 000 /sbin/apk 2>/dev/null || true
mount -o remount,ro /`,

	"moderate": `chmod 000 /sbin/apk 2>/dev/null || true
echo "Moderate seccomp profile applied"`,

	"permissive": `echo "Permissive seccomp profile — no restrictions, audit only"`,
}

// maxVolumeCopyBytes caps the raw (pre-base64) size of a single volume's tar
// archive. The agent protocol's frame size is capped at 10 MiB
// (agent/internal/protocol/frame.go, internal/agentclient/protocol.go's
// maxFrameSize) and the whole request — command string + base64 stdin — is
// one frame. Base64 inflates by ~4/3; 6 MiB raw -> ~8 MiB encoded, leaving
// headroom for the JSON envelope under the 10 MiB cap.
//
// This is the concrete shape of Firecracker's copy-in trade-off: fine for
// source trees and config, not a general bind-mount replacement. Anything
// bigger needs applevz's live virtiofs share instead.
const maxVolumeCopyBytes = 6 * 1024 * 1024

// SetupVolumeMounts copies each host directory into the guest at boot via a
// one-shot tar transfer over the guest agent's vsock connection — NOT a live
// mount. Firecracker's VMM has never implemented virtio-fs or 9p, so there is
// no live host<->guest filesystem sharing available on this backend; changes
// made on either side after this call are never synced. See the "applevz"
// backend (internal/vm/applevz.go, internal/cli/start.go's runStartAppleVZ)
// for the live-mount path.
//
// Every volume's format is validated up front, before any network I/O, so a
// single malformed -V entry fails fast without partially copying others.
func SetupVolumeMounts(vm *state.VM, volumes []string) error {
	type parsedVolume struct{ hostPath, guestPath string }

	parsed := make([]parsedVolume, 0, len(volumes))
	for _, vol := range volumes {
		parts := strings.SplitN(vol, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid volume format %q (expected hostPath:guestPath)", vol)
		}
		parsed = append(parsed, parsedVolume{hostPath: parts[0], guestPath: parts[1]})
	}

	for _, v := range parsed {
		if err := copyDirIntoGuest(vm.Name, v.hostPath, v.guestPath); err != nil {
			return fmt.Errorf("volume %s:%s: %w", v.hostPath, v.guestPath, err)
		}
	}
	return nil
}

// copyDirIntoGuest tars hostPath, base64-encodes it, and ships it to the
// guest agent's non-interactive Exec (internal/agentclient/client.go's
// Client.Exec) with a command that decodes and extracts it under guestPath.
// Exec's stdin is a plain JSON string field (agent/internal/protocol/frame.go),
// not a raw byte channel, so base64 is required here — sending raw tar bytes
// through a JSON string would corrupt any non-UTF-8 byte.
func copyDirIntoGuest(vmName, hostPath, guestPath string) error {
	data, err := buildTarArchive(hostPath)
	if err != nil {
		return err
	}

	client := agentclient.New(&agentclient.FirecrackerVsockDialer{UDSPath: VsockUDSPath(vmName)})
	cmd := fmt.Sprintf("mkdir -p %s && base64 -d | tar -xf - -C %s",
		shellQuoteForSSH(guestPath), shellQuoteForSSH(guestPath))

	// No explicit deadline: Client.exchange applies agentclient.DefaultRequestTimeout
	// (5 minutes) when ctx has none — plenty for a <=6MiB transfer.
	result, err := client.Exec(context.Background(), cmd, base64.StdEncoding.EncodeToString(data))
	if err != nil {
		return fmt.Errorf("copy into guest: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("tar extract exited %d: %s", result.ExitCode, result.Output)
	}
	return nil
}

// buildTarArchive tars the contents of hostDir (paths relative to hostDir,
// suitable for extraction with `tar -C <dest>`), enforcing maxVolumeCopyBytes
// before the archive is ever sent anywhere.
func buildTarArchive(hostDir string) ([]byte, error) {
	info, err := os.Stat(hostDir)
	if err != nil {
		return nil, fmt.Errorf("stat host path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("host path %q is not a directory", hostDir)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err = filepath.WalkDir(hostDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == hostDir {
			return nil
		}
		rel, err := filepath.Rel(hostDir, path)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		if buf.Len() > maxVolumeCopyBytes {
			return fmt.Errorf("host directory %q exceeds the %d-byte Firecracker copy-in limit (use applevz for larger shares)", hostDir, maxVolumeCopyBytes)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > maxVolumeCopyBytes {
		return nil, fmt.Errorf("host directory %q exceeds the %d-byte Firecracker copy-in limit (use applevz for larger shares)", hostDir, maxVolumeCopyBytes)
	}
	return buf.Bytes(), nil
}

func shellQuoteForSSH(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
```

Note: `time.Duration`/`time` import above is unused in this version — drop it from the import list (the `context.Background()` + `agentclient.DefaultRequestTimeout` combination doesn't need it locally). Run `goimports -w internal/firecracker/security.go` or remove it by hand before building.

Update the one caller in `internal/server/routes.go:262-266`:
```go
		if len(req.Volumes) > 0 {
			if err := firecracker.SetupVolumeMounts(postVM, req.Volumes); err != nil {
				log.Printf("VM %s: volume mount setup failed: %v", req.Name, err)
			}
		}
```
(drops the leading `s.executor` argument — everything else on that line is unchanged).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/firecracker/ -run 'TestBuildTarArchive|TestSetupVolumeMounts' -v && go test ./internal/server/ -v`
Expected: PASS, clean build. The `internal/server` run confirms `routes_test.go`'s existing volume-flow tests (`TestSpecFromCreateRequest` at line 775, `TestHandleInspectVM`) still pass unmodified — they test request/spec shaping, not `SetupVolumeMounts` itself, so the signature change shouldn't touch them, but confirm.

- [ ] **Step 5: Commit**

```bash
git add internal/firecracker/security.go internal/firecracker/network_test.go internal/server/routes.go
git commit -m "feat(firecracker): real tar copy-in for -V volumes, replacing the mkdir-only stub"
```

---

### Task 4 (CONTINGENT on Task 1 Step 3): Custom applevz kernel with virtio-fs support

**Only execute this task if Task 1, Step 3 showed `unknown filesystem type 'virtiofs'` (no kernel driver). If Step 3 showed the mount syscall reaching the driver, skip straight to Task 5 — do not build a kernel.**

**Files:**
- Create: `internal/firecracker/scripts/build-applevz-kernel.sh` (confirm final location against Step 1's findings; this default mirrors `build-rootfs.sh`'s existing pattern of a script copied into Lima and run there, since arm64 kernel cross-compilation needs a Linux toolchain and Lima already provides one)
- Modify: `internal/cli/start.go`'s `runStartAppleVZ` (`kernelPath` currently hardcoded to the shared `~/.mvm/cache/vmlinux` at line 235 — applevz needs its own kernel path once it diverges from Firecracker's)

- [ ] **Step 1: Locate Firecracker's own CI kernel config as a starting point**

Run:
```bash
git clone --depth 1 https://github.com/firecracker-microvm/firecracker.git /tmp/fc-src
find /tmp/fc-src/resources/guest_configs -iname '*aarch64*6.1*' -o -iname '*arm64*6.1*'
```
Expected: a config fragment (exact filename to be confirmed by this command — Firecracker's repo layout may have moved between versions) that was used to build the `vmlinux-6.1.*` binary this project downloads (`internal/firecracker/scripts/build-rootfs.sh:16-22`). This is the base to extend, not build from scratch — it already has virtio-net/blk/vsock/balloon correctly configured for this exact boot path (VZLinuxBootLoader boots a raw ELF/Image kernel the same way Firecracker does — no bzImage/GRUB involved on either backend).

- [ ] **Step 2: Add virtio-fs + FUSE to the config, build, verify**

Using Lima (the same Debian arm64 environment `build-rootfs.sh` already runs in) as the build host:
```bash
limactl shell <lima-vm-name> -- bash -c '
  set -e
  sudo apt-get install -y build-essential bc flex bison libssl-dev libelf-dev
  git clone --depth 1 --branch v6.1 https://github.com/torvalds/linux.git /tmp/linux-vfs
  cp /tmp/fc-src/resources/guest_configs/<the-config-found-in-step-1> /tmp/linux-vfs/.config
  cd /tmp/linux-vfs
  ./scripts/config --enable CONFIG_FUSE_FS --enable CONFIG_VIRTIO_FS
  make ARCH=arm64 olddefconfig
  make ARCH=arm64 -j$(nproc) Image
'
```
Copy the resulting `Image` out to `~/.mvm/cache/vmlinux-applevz` (a *separate* file from the shared `vmlinux`, so the Firecracker backend's kernel — which must stay minimal and matched to what its own CI validated — is untouched).

Verify with the same command as Task 1 Step 3, pointed at a VM booted from the new kernel: expect the mount attempt to now fail with a *device*-level error (`Invalid argument` / "no such device", since no `--share` is attached to this test VM) rather than `unknown filesystem type`.

- [ ] **Step 3: Point applevz at the new kernel**

In `internal/cli/start.go`'s `runStartAppleVZ`, change:
```go
	kernelPath := filepath.Join(cacheDir, "vmlinux")
```
to:
```go
	kernelPath := filepath.Join(cacheDir, "vmlinux-applevz")
```
(line 235 today; Firecracker's `kernelPath` construction, wherever it lives in `internal/firecracker/install.go`/`config.go`, is untouched — the two backends now intentionally diverge).

- [ ] **Step 4: Commit**

```bash
git add internal/firecracker/scripts/build-applevz-kernel.sh internal/cli/start.go
git commit -m "feat(applevz): build a separate kernel with virtio-fs support for volume mounts"
```

---

### Task 5: applevz Swift — deterministic virtio-fs tags

**Files:**
- Modify: `vz/Sources/mvm-vz/Commands/Create.swift`

**Interfaces:**
- Produces: tags of the exact form `"vol0"`, `"vol1"`, ... assigned by the position of each `--share hostPath:guestPath` argument in the order `internal/vm/applevz.go:147-150`'s loop emits them (which is the same order `volumes []string` arrives in from the CLI). Task 6's Go-side mount step depends on this exact scheme matching — they are not otherwise coordinated (no tag is threaded back through the `vzCreateResult` JSON status line), so a change to one side without the other silently breaks mounting.

- [ ] **Step 1: Fix the share-parsing loop**

In `vz/Sources/mvm-vz/Commands/Create.swift`, replace lines 53-66:
```swift
        // Parse share options into (tag, hostPath) tuples.
        // NOTE: the existing semantics (parts[0]=hostPath, parts[1]=tag)
        // match what internal/vm/applevz.go passes today. The volume-mount
        // feature is not yet end-to-end functional on either backend; see
        // the bonus-bug note in PR #1's commit message and the follow-up
        // issue for fixing virtiofs guest-side mount plumbing.
        var shares: [(tag: String, hostPath: String)] = []
        for s in share {
            let parts = s.split(separator: ":", maxSplits: 1)
            if parts.count == 2 {
                shares.append((tag: String(parts[1]), hostPath: String(parts[0])))
            }
        }
```
with:
```swift
        // Parse share options into (tag, hostPath) tuples. --share is
        // "hostPath:guestPath" (see internal/vm/applevz.go's StartVM, which
        // passes -V volumes through unmodified). guestPath is NOT used as the
        // virtio-fs tag: VZVirtioFileSystemDeviceConfiguration's tag is meant
        // to be a short opaque identifier, not a filesystem path, and nothing
        // here needs the guest path at all — the guest side (Go, over the
        // agent, after boot) does its own mkdir+mount using the guest path it
        // already has from the same -V flag. What the guest DOES need is the
        // tag, and it derives it the same deterministic way: "vol<index>" by
        // position in this list. Both sides must stay in lockstep on this
        // scheme — see internal/cli/start.go's virtiofsMountCommands.
        var shares: [(tag: String, hostPath: String)] = []
        for (index, s) in share.enumerated() {
            let parts = s.split(separator: ":", maxSplits: 1)
            if parts.count == 2 {
                shares.append((tag: "vol\(index)", hostPath: String(parts[0])))
            }
        }
```

- [ ] **Step 2: Build**

Run:
```bash
cd /Users/paulmeller/Projects/firecracker/vz
swift build -c release
```
Expected: clean build. Confirm the built binary is where `internal/vzhelper.HelperBinary()` expects it — `grep -n HelperBinary internal/vzhelper/*.go` — and copy/symlink if the build output path differs (matches whatever the existing dev workflow already does for `mvm-vz` changes; check `scripts/` or `Makefile` at repo root for an existing "rebuild vz helper" step before inventing a new one).

- [ ] **Step 3: Manual verification (no local Swift test harness exists — see Global Constraints)**

Run:
```bash
mvm start tag-fix-check -V /tmp/vol-verify-src:/data
cat ~/.mvm/vms/tag-fix-check/mvm-vz-stderr.log 2>/dev/null
mvm delete tag-fix-check
```
Expected: VM boots successfully (no `vzConfig.validate()` throw in the stderr log) — this only confirms the tag is now well-formed; the guest still won't have `/data` populated until Task 6 lands (there is no guest-side mount yet).

- [ ] **Step 4: Commit**

```bash
git add vz/Sources/mvm-vz/Commands/Create.swift
git commit -m "fix(applevz): derive virtio-fs tags deterministically instead of misusing the guest path"
```

---

### Task 6: applevz Go — guest-side virtio-fs mount

**Files:**
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/start_test.go`

**Interfaces:**
- Consumes: `shellQuote` (`internal/cli/exec.go:249`, already in package `cli` — no import needed), `agent.Exec` (`internal/agentclient/client.go:99-119`, already used identically at `internal/cli/start.go:378`).
- Produces: `func virtiofsMountCommands(volumes []string) ([]string, error)`. `runStartAppleVZ` calls it once, in order, right after network-policy setup.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/start_test.go`:

```go
// === virtiofsMountCommands ===

func TestVirtiofsMountCommands(t *testing.T) {
	cmds, err := virtiofsMountCommands([]string{"/host/a:/data", "/host/b:/app/lib"})
	if err != nil {
		t.Fatalf("virtiofsMountCommands: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2", len(cmds))
	}
	// Tag order (vol0, vol1, ...) must match Create.swift's share-parsing
	// loop exactly — see the comment there.
	if !strings.Contains(cmds[0], "vol0") || !strings.Contains(cmds[0], "/data") {
		t.Errorf("cmds[0] = %q, want references to vol0 and /data", cmds[0])
	}
	if !strings.Contains(cmds[1], "vol1") || !strings.Contains(cmds[1], "/app/lib") {
		t.Errorf("cmds[1] = %q, want references to vol1 and /app/lib", cmds[1])
	}
}

func TestVirtiofsMountCommandsEmpty(t *testing.T) {
	cmds, err := virtiofsMountCommands(nil)
	if err != nil || len(cmds) != 0 {
		t.Errorf("virtiofsMountCommands(nil) = %v, %v; want no commands, no error", cmds, err)
	}
}

func TestVirtiofsMountCommandsInvalidFormat(t *testing.T) {
	_, err := virtiofsMountCommands([]string{"no-colon-here"})
	if err == nil {
		t.Error("want error for a volume missing the guest-path colon")
	}
}
```

Add `"strings"` to `start_test.go`'s imports if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestVirtiofsMountCommands -v`
Expected: FAIL — `undefined: virtiofsMountCommands`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/cli/start.go`, near `applevzSpec` (after line 213):

```go
// virtiofsMountCommands returns, in order, the shell command that mounts
// each already-validated "hostPath:guestPath" volume inside the guest via
// virtio-fs. Tags are assigned "vol0", "vol1", ... by position — this must
// match vz/Sources/mvm-vz/Commands/Create.swift's share-parsing loop exactly,
// since the tag is never threaded back through the mvm-vz status line; both
// sides derive it independently from the same ordering.
func virtiofsMountCommands(volumes []string) ([]string, error) {
	var cmds []string
	for i, v := range volumes {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid volume format %q (expected hostPath:guestPath)", v)
		}
		guestPath := parts[1]
		tag := fmt.Sprintf("vol%d", i)
		cmds = append(cmds, fmt.Sprintf("mkdir -p %s && mount -t virtiofs %s %s",
			shellQuote(guestPath), tag, shellQuote(guestPath)))
	}
	return cmds, nil
}
```

Wire it into `runStartAppleVZ`. Insert right after the network-policy block and before the `timer.mark("net_setup")` line (currently line 408, inside the `if agentReady {` block that starts at line 369):

```go
			// Mount each -V share via virtio-fs. Depends on the tags assigned
			// in vz/Sources/mvm-vz/Commands/Create.swift's share-parsing loop
			// matching this exact order — see virtiofsMountCommands's comment.
			if mountCmds, err := virtiofsMountCommands(volumes); err != nil {
				logf("  Warning: invalid volume spec: %v\n", err)
			} else {
				for _, mc := range mountCmds {
					if _, err := agent.Exec(ctx, mc, ""); err != nil {
						logf("  Warning: mount volume: %v\n", err)
					}
				}
			}
```

Place this immediately before the existing `// Apply network policy via the agent.` block's closing, i.e. right after the `applyVZNetworkPolicy` call and before `timer.mark("net_setup")` — both run inside the same 30-second `ctx` already established at line 370, so no new context is needed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v && go build ./...`
Expected: PASS (all `internal/cli` tests, including the three new ones and every pre-existing test unmodified), clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go
git commit -m "feat(applevz): mount -V shares via virtio-fs after boot, matching Create.swift's tag scheme"
```

---

### Task 7: `mvm inspect` reflects volumes — regression test only

No production code change is needed: `state.VMSpec.Volumes` (`internal/state/spec.go:16`) is already populated by both `specFromCreateRequest` (`internal/server/routes.go:100-110`, Firecracker) and `applevzSpec` (`internal/cli/start.go:198-213`, applevz), and `InspectResponseFromVM` (`internal/server/routes.go:83-96`) passes the whole `*state.VMSpec` pointer through unmodified — `TestSpecFromCreateRequest` (`internal/server/routes_test.go:775-797`) already asserts `spec.Volumes` is captured correctly. What's missing is a regression test proving `Volumes` specifically survives the inspect round-trip (existing inspect tests only exercise `Cpus`/`NetPolicy`), so a future refactor that narrows `VMInspectResponse` to explicit fields (instead of embedding `*state.VMSpec`) wouldn't silently drop it unnoticed.

**Files:**
- Modify: `internal/server/routes_test.go`

**Interfaces:** none — test-only, exercises existing `InspectResponseFromVM` and `handleCreateVM`/`GET /vms/{name}`.

- [ ] **Step 1: Write the test**

Extend `TestInspectResponseFromVM` (`internal/server/routes_test.go:843-...`) — add `Volumes: []string{"/host:/guest"}` to the `Spec` literal at line 853, and after the existing assertions add:
```go
	if len(resp.Spec.Volumes) != 1 || resp.Spec.Volumes[0] != "/host:/guest" {
		t.Errorf("resp.Spec.Volumes = %v, want [/host:/guest]", resp.Spec.Volumes)
	}
```

- [ ] **Step 2: Run to confirm it passes without any production change**

Run: `go test ./internal/server/ -run TestInspectResponseFromVM -v`
Expected: PASS immediately — this is the point of the task: prove the plumbing already works and pin it with a test, not fix a bug.

- [ ] **Step 3: Commit**

```bash
git add internal/server/routes_test.go
git commit -m "test(server): pin that mvm inspect surfaces -V volumes, not just cpus/net-policy"
```

---

### Task 8: Manual end-to-end verification (both backends, post-fix)

**Files:** none.

- [ ] **Step 1: Firecracker — copy-in works, and its documented limits hold**

```bash
mkdir -p /tmp/vol-e2e && echo "host-write-before-boot" > /tmp/vol-e2e/marker.txt
mvm run base -V /tmp/vol-e2e:/data -- cat /data/marker.txt
```
Expected: prints `host-write-before-boot`. Then confirm the documented non-live limit:
```bash
mvm start fc-e2e -V /tmp/vol-e2e:/data
echo "host-write-after-boot" >> /tmp/vol-e2e/marker.txt
mvm exec fc-e2e -- cat /data/marker.txt   # expect: still just "host-write-before-boot" — no live sync
mvm exec fc-e2e -- sh -c 'echo guest-write >> /data/marker.txt'
cat /tmp/vol-e2e/marker.txt               # expect: unchanged on host — no copy-out
mvm delete fc-e2e
```

- [ ] **Step 2: applevz — live and bidirectional**

```bash
mvm start vz-e2e -V /tmp/vol-e2e:/data
mvm exec vz-e2e -- cat /data/marker.txt          # sees current host content
echo "host-write-live" >> /tmp/vol-e2e/marker.txt
mvm exec vz-e2e -- cat /data/marker.txt          # expect: includes host-write-live, no re-mount needed
mvm exec vz-e2e -- sh -c 'echo guest-write-live >> /data/marker.txt'
cat /tmp/vol-e2e/marker.txt                       # expect: includes guest-write-live
mvm delete vz-e2e
```
Expected on both directions: changes appear without any additional command — this is the bar for "live."

- [ ] **Step 3: `mvm inspect` shows volumes on both backends**

```bash
mvm start inspect-fc -V /tmp/vol-e2e:/data && mvm inspect inspect-fc | grep -A2 volumes && mvm delete inspect-fc
mvm start inspect-vz -V /tmp/vol-e2e:/data && mvm inspect inspect-vz | grep -A2 volumes && mvm delete inspect-vz
```
Expected: both show `"volumes": ["/tmp/vol-e2e:/data"]` in the JSON output.

---

### Task 9: Full-suite verification

**Files:** none.

- [ ] **Step 1: Full build, vet, test**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: clean build, silent vet, every package `ok` (hardware-dependent tests may skip; no FAILs).

- [ ] **Step 2: Confirm no regression in the commands this plan touched**

Run: `go test ./internal/cli/ -run 'TestParsePorts|TestParseVolumes|TestVirtiofsMountCommands|TestApplevzSpec|TestRunStart|TestRunExec|TestRunDelete' -v && go test ./internal/firecracker/ -v && go test ./internal/server/ -v`
Expected: PASS across the board.

- [ ] **Step 3: Commit (only if Steps 1-2 required a fix)**

If everything already passed clean, skip. Otherwise:
```bash
git add -A
git commit -m "fix: address full-suite verification findings for volume mounts"
```

---

## Out of Scope (explicitly)

- **Firecracker live/bidirectional sync.** No `mvm cp`, no `mvm start --watch`-style rsync-back for `-V` (note: `--watch` already exists for a different purpose — build-triggered rebuilds, see `internal/cli/start.go:77` — not related to this plan's volumes). A future plan could add explicit copy-out (`mvm cp <name>:<guestPath> <hostPath>`) using the same `agentclient.Client.Exec` primitive in reverse (`tar -cf -` guest-side, base64 back to the host) — deliberately not bundled here to keep this plan's Firecracker deliverable to one clear, testable behavior.
- **Volumes larger than ~6 MiB on Firecracker.** Bounded by the agent protocol's 10 MiB frame cap (Global Constraints). Not addressed by chunking/streaming in this plan — that's a real option for later (the agent protocol would need a multi-frame transfer mode) but adds real complexity for a workload applevz already covers live.
- **Remote/cloud daemon host-path resolution.** `parseVolumes` (Task 2) assumes the CLI and the machine performing the copy/mount share a filesystem view (true for local macOS+Lima and local applevz; not true for the remote-CLI-to-cloud-KVM-box setup from commit `c0cdd21`).
- **Kernel rebuild pipeline productionization** (Task 4, if triggered): this plan gets a working custom kernel built and verified once; wiring it into the existing `mvm doctor`/first-run install flow (`internal/firecracker/install.go`, `docs/superpowers/plans/` setup docs) so new installs get it automatically is follow-up work.
- **Excluding `.git`/`node_modules`/build artifacts from the Firecracker tar.** `buildTarArchive` copies everything under `hostPath`; given the 6 MiB cap, a directory containing `node_modules` will simply fail the size check with a clear error rather than silently truncating — acceptable for now, but a `.mvmignore`-style exclude list would raise the cap's practical ceiling considerably if this becomes a common complaint.
