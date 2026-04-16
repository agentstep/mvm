# UFFD-Based Memory Page-In for Firecracker Snapshot Restore — Research Report

**Author:** Research agent
**Date:** 2026-04-15
**Goal:** Replace current `File` mem backend (~12s restore for 2GB VM) with `Uffd` lazy page-in (~30ms) by implementing a Go UFFD handler that runs inside the Lima VM alongside the daemon.

---

## TL;DR

1. Firecracker **v1.13 supports UFFD** (stable since v1.1, refined through v1.8). No version bump required.
2. The API change is trivial: flip `"backend_type": "File"` to `"Uffd"` and point `backend_path` at a unix socket.
3. Wire protocol is simple: one SCM_RIGHTS message carrying a JSON array of `GuestRegionUffdMapping` structs as the message body plus one file descriptor (the UFFD fd) as an ancillary control message.
4. A minimal Go handler is ~300-400 LOC using `golang.org/x/sys/unix`. No mature Go UFFD wrapper exists that we'd want in production — we should write our own thin wrapper (UFFDIO_API, UFFDIO_REGISTER, UFFDIO_COPY are three ioctls).
5. Expected result for a 2GB guest: restore latency drops from ~12s to <100ms. Amazon's own docs cite 7s → <100ms for their 2GB benchmark on GCE n2-standard-8.
6. Biggest gotcha: if the handler dies, the guest hangs forever on its next page fault. Need supervision + SO_PEERCRED linkage.

---

## 1. Firecracker API Payload

### Current (File backend)
From `internal/firecracker/snapshot.go:182-188`:
```json
{
  "snapshot_path": "/var/lib/mvm/<vm>/snapshot.bin",
  "mem_backend": {
    "backend_path": "/var/lib/mvm/<vm>/mem.bin",
    "backend_type": "File"
  },
  "enable_diff_snapshots": false,
  "resume_vm": true,
  "network_overrides": [{"iface_id": "net1", "host_dev_name": "tap0"}]
}
```

### New (UFFD backend)
```json
{
  "snapshot_path": "/var/lib/mvm/<vm>/snapshot.bin",
  "mem_backend": {
    "backend_path": "/var/lib/mvm/<vm>/uffd.sock",
    "backend_type": "Uffd"
  },
  "enable_diff_snapshots": false,
  "resume_vm": true,
  "network_overrides": [{"iface_id": "net1", "host_dev_name": "tap0"}]
}
```

**Exact field names (verified against Firecracker docs):**
- `snapshot_path` (string) — path to `snapshot.bin`
- `mem_backend.backend_type` — `"File"` or `"Uffd"` (case-sensitive, capital U)
- `mem_backend.backend_path` — for UFFD: path to a **unix domain socket** that the handler has already `bind()`ed and is `listen()`ing on *before* the API call is made
- `resume_vm` (bool)
- `enable_diff_snapshots` is deprecated in v1.13 in favor of `track_dirty_pages` (doesn't matter for load, only create)

**Mutual exclusivity:** do not set both `mem_backend` and the deprecated `mem_file_path`. Firecracker rejects it.

**Version confirmation:** Firecracker v1.13.0 (the version pinned in `internal/firecracker/install.go:20`) supports UFFD. First introduced in v1.1 (2022). The v1.5 release added `/dev/userfaultfd` support (kernel ≥ 6.1). v1.8 fixed a forward-compat issue with Linux 6.6. No breaking changes to the UFFD protocol since then.

---

## 2. UFFD Socket Protocol — Exact Wire Format

This is the non-obvious part. The Firecracker docs describe it vaguely; the authoritative source is the example handler in the Firecracker repo (`src/firecracker/examples/uffd/uffd_utils.rs`).

### Sequence

1. **Handler side (before API call):**
   - `bind()` a Unix domain socket at `backend_path` (e.g. `/var/lib/mvm/<vm>/uffd.sock`)
   - `listen()`
   - `mmap()` the memory dump file `PROT_READ | MAP_PRIVATE` (optionally `MAP_POPULATE`) — this is the "backing buffer"

2. **Orchestrator issues `PUT /snapshot/load`** with UFFD backend.

3. **Firecracker side:**
   - Creates the UFFD object via `/dev/userfaultfd` (kernel ≥ 6.1, which we have in Lima's default Ubuntu 24.04 template) OR via the `userfaultfd(2)` syscall fallback.
   - `mmap(MAP_ANONYMOUS | MAP_PRIVATE)` each guest memory region.
   - Registers all regions with the UFFD via `UFFDIO_REGISTER` with `UFFDIO_REGISTER_MODE_MISSING`.
   - Connects to the unix socket.
   - Sends **one message** containing:
     - **Body:** JSON-serialized `Vec<GuestRegionUffdMapping>` (UTF-8)
     - **Control message (SCM_RIGHTS):** the UFFD file descriptor

4. **Handler side:**
   - `recvmsg()` on the accepted stream. Parse the body JSON. Extract the UFFD fd from the cmsg.
   - **No further messages on this socket.** The socket stays open (used by Firecracker's `SO_PEERCRED` trick for lifecycle signaling) but carries no data.

5. **Page-fault loop:**
   - `poll()` the UFFD fd for `POLLIN`.
   - `read()` a `struct uffd_msg` (48 bytes on x86_64/aarch64).
   - If `event == UFFD_EVENT_PAGEFAULT`: compute the guest-region that contains the fault address, compute the offset into the backing mmap, issue `UFFDIO_COPY` to populate the page.
   - If `event == UFFD_EVENT_REMOVE`: call `UFFDIO_UNREGISTER` on that range. (Only happens with balloon device — we don't use balloon, so this is defensive.)

### The message format — critical details

From `uffd_utils.rs:32-42`, the JSON schema is:
```rust
#[derive(Serialize, Deserialize)]
pub struct GuestRegionUffdMapping {
    pub base_host_virt_addr: u64,  // where Firecracker mmap'd the region
    pub size: usize,               // region size in bytes
    pub offset: u64,               // offset in mem.bin where this region's data lives
    pub page_size: usize,          // always 4096 unless hugepages configured
}
```

Wire example (body):
```json
[
  {"base_host_virt_addr":140234567680000,"size":2147483648,"offset":0,"page_size":4096}
]
```
(Usually one region for a normal VM; multiple regions if there's an MMIO gap between low and high memory, which happens past 3GB on x86.)

**Note:** `page_size` replaced the older `page_size_kib` (which confusingly held bytes, not KiB) in recent Firecracker versions. v1.13 emits the new name. Handle both if we want backward compat, but for pinning to v1.13 we can assume `page_size` only.

### The `send_with_fd`/`recv_with_fd` call in Rust

Firecracker uses `vmm_sys_util::sock_ctrl_msg::ScmSocket`:
```rust
stream.send_with_fd(json_bytes, uffd_raw_fd)
```
This is `sendmsg(2)` with:
- `msg_iov` = the JSON bytes
- `msg_control` = a single `cmsghdr` with `cmsg_level=SOL_SOCKET`, `cmsg_type=SCM_RIGHTS`, payload = `[uffd_fd]`

### Retry quirk

`uffd_utils.rs:77-98` shows Firecracker/the handler occasionally fail to receive the fd on the first try and retry up to 5× with 100ms sleeps. Observed behavior: sometimes the data arrives but the cmsg doesn't. Our handler must do the same retry dance.

---

## 3. Minimal Go UFFD Handler — Design

### File layout
```
internal/uffd/
├── uffd.go          # syscall wrappers: ApiHandshake, Register, Copy (thin ioctl calls)
├── handler.go       # main event loop: accept, parse mappings, mmap backing file, serve_pf
├── cmd/mvm-uffd/    # standalone binary
│   └── main.go      # argv: <socket-path> <mem-file-path>
└── uffd_test.go
```

### Syscall primitives (no external deps beyond golang.org/x/sys/unix, already vendored at v0.43.0)

```go
package uffd

import (
    "syscall"
    "unsafe"
    "golang.org/x/sys/unix"
)

// Constants from <linux/userfaultfd.h>. Stable ABI.
const (
    UFFD_API = 0xAA

    // ioctl numbers — encoded via _IOWR('U', n, size) / _IOR macros
    UFFDIO_API       = 0xc018aa3f
    UFFDIO_REGISTER  = 0xc020aa00
    UFFDIO_COPY      = 0xc028aa03
    UFFDIO_ZEROPAGE  = 0xc020aa04
    UFFDIO_WAKE      = 0x8010aa02
    UFFDIO_UNREGISTER = 0x8010aa01

    UFFDIO_REGISTER_MODE_MISSING = 1 << 0

    UFFD_EVENT_PAGEFAULT = 0x12
    UFFD_EVENT_REMOVE    = 0x14
)

// Structs must exactly match kernel layout. Verify with `pahole` or cross-check
// against <linux/userfaultfd.h>.

type uffdioAPI struct {
    API      uint64
    Features uint64
    Ioctls   uint64
}

type uffdioRange struct {
    Start uint64
    Len   uint64
}

type uffdioRegister struct {
    Range  uffdioRange
    Mode   uint64
    Ioctls uint64
}

type uffdioCopy struct {
    Dst       uint64
    Src       uint64
    Len       uint64
    Mode      uint64
    Copy      int64  // return value
}

// uffdMsg is 48 bytes. Use a union approximation.
type uffdMsg struct {
    Event    uint8
    _        [7]byte  // reserved + pad + pad + pad
    Arg      [40]byte // union — we'll reinterpret for pagefault
}

type uffdPagefault struct {
    Flags   uint64
    Address uint64
    Ptid    uint32
    _       uint32
}

// ApiHandshake is called once after opening /dev/userfaultfd (or the syscall fd).
func ApiHandshake(fd int) error {
    api := uffdioAPI{API: UFFD_API}
    _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), UFFDIO_API, uintptr(unsafe.Pointer(&api)))
    if e != 0 { return e }
    return nil
}

func Copy(fd int, dst, src, length uintptr) (int64, error) {
    c := uffdioCopy{Dst: uint64(dst), Src: uint64(src), Len: uint64(length)}
    _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), UFFDIO_COPY, uintptr(unsafe.Pointer(&c)))
    if e != 0 { return c.Copy, e }
    return c.Copy, nil
}
```

### Main handler loop

```go
func (h *Handler) Run() error {
    // 1. bind socket
    l, err := net.Listen("unix", h.SocketPath)
    if err != nil { return err }
    defer l.Close()

    // 2. accept connection from Firecracker
    conn, err := l.Accept()
    if err != nil { return err }
    uc := conn.(*net.UnixConn)

    // 3. mmap backing file PROT_READ|MAP_PRIVATE|MAP_POPULATE
    backing, err := mmapFile(h.MemFilePath)
    if err != nil { return err }

    // 4. recvmsg loop (retry up to 5x to handle the "no fd" quirk)
    var uffdFd int
    var mappings []GuestRegionUffdMapping
    for attempt := 0; attempt < 5; attempt++ {
        body, fd, err := recvWithFd(uc)
        if err == nil && fd != -1 {
            if err := json.Unmarshal(body, &mappings); err == nil {
                uffdFd = fd
                break
            }
        }
        time.Sleep(100 * time.Millisecond)
    }
    if uffdFd == 0 { return errors.New("uffd fd not received after 5 retries") }

    // 5. Install panic/exit hook — get Firecracker's pid via SO_PEERCRED
    fcPid := getPeerPid(uc)

    // 6. Poll loop
    pageSize := uintptr(mappings[0].PageSize)
    for {
        ev, err := readUffdMsg(uffdFd)
        if err != nil { return err }
        switch ev.Event {
        case UFFD_EVENT_PAGEFAULT:
            pf := (*uffdPagefault)(unsafe.Pointer(&ev.Arg[0]))
            faultAddr := uintptr(pf.Address) &^ (pageSize - 1)
            region := findRegion(mappings, uint64(faultAddr))
            offset := uint64(faultAddr) - region.BaseHostVirtAddr
            src := uintptr(unsafe.Pointer(&backing[region.Offset + offset]))
            if _, err := Copy(uffdFd, faultAddr, src, pageSize); err != nil {
                if err == unix.EAGAIN { /* retry next round */ }
                if err == unix.EEXIST { /* already resolved — benign */ continue }
                return err
            }
        case UFFD_EVENT_REMOVE:
            // Only balloon triggers this; we don't use balloon. Unregister.
        }
    }
}
```

### SCM_RIGHTS receive in Go

`golang.org/x/sys/unix` has the primitives:
```go
func recvWithFd(uc *net.UnixConn) ([]byte, int, error) {
    oob := make([]byte, unix.CmsgSpace(4))  // space for one int (fd)
    buf := make([]byte, 4096)
    n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
    if err != nil { return nil, -1, err }
    scms, err := unix.ParseSocketControlMessage(oob[:oobn])
    if err != nil { return nil, -1, err }
    for _, scm := range scms {
        if scm.Header.Level == unix.SOL_SOCKET && scm.Header.Type == unix.SCM_RIGHTS {
            fds, err := unix.ParseUnixRights(&scm)
            if err == nil && len(fds) >= 1 {
                return buf[:n], fds[0], nil
            }
        }
    }
    return buf[:n], -1, nil
}
```

### Opening UFFD (handler side — wait, actually not needed)

Crucial reminder: **the handler does not open `/dev/userfaultfd`**. Firecracker does that and sends the already-opened fd to us via SCM_RIGHTS. We just receive and use it. This also means the handler does not need `CAP_SYS_PTRACE` or special permissions on `/dev/userfaultfd` — only Firecracker does.

### Estimated LOC
- `uffd.go` (syscall wrappers, constants, structs): ~120 LOC
- `handler.go` (loop, mmap, recvmsg, find region): ~180 LOC
- `cmd/mvm-uffd/main.go` (argv parsing, signal handling): ~50 LOC
- Tests: ~150 LOC

Total: **~450 LOC**, conservative. The 300 LOC estimate is achievable if we fold cmd/ into handler.go and keep tests separate.

### Go libraries reviewed (and why I recommend rolling our own)

| Library | Verdict |
|---|---|
| `github.com/loopholelabs/userfaultfd-go` | Abandoned (1 release in 2024, 9 stars). Opinionated — wraps `io.ReaderAt` rather than exposing raw ioctls. Not a fit. |
| `github.com/ricardobranco777/go-userfaultfd` | Pre-v1.0, 0 imports. Has the right primitives (UFFDIO_COPY etc) and is closer to what we want, but we'd be the only users. |
| `golang.org/x/sys/unix` | Has `SYS_USERFAULTFD`, `unix.Ioctl*` helpers, SCM_RIGHTS parsing. No userfaultfd-specific helpers but provides everything we need. **Recommended.** |

Writing the ~150 LOC of ioctl wrappers ourselves gives us: stable API, vetted code, no third-party supply chain risk, and the ability to tweak the struct layouts if the kernel ever adds fields (it does — Linux 6.6 added a `UFFDIO_POISON` ioctl).

---

## 4. Lifecycle — How the Handler Is Supervised

This is the most important non-code problem. **If the handler dies while the VM is running, the next guest page fault hangs forever.** Firecracker is deliberately blocking on the UFFD fd; there's no timeout.

### Our supervision model

The mvm daemon already supervises Firecracker processes. We extend it to supervise a sibling UFFD handler per VM.

**Per-VM state (in memory, daemon-side):**
```go
type VM struct {
    ...
    UFFDPid    int        // pid of mvm-uffd process
    UFFDSocket string     // path to the uffd.sock
}
```

**Startup sequence (inside `loadSnapshot`):**

```
1. daemon starts: mvm-uffd --socket=/var/lib/mvm/<vm>/uffd.sock --mem=/var/lib/mvm/<vm>/mem.bin
2. daemon waits (up to 2s) for socket to appear and be ready for accept()
   (poll for existence; the handler's bind+listen is effectively instant)
3. daemon issues PUT /snapshot/load with backend_type=Uffd, backend_path=<socket>
4. daemon records UFFDPid in state
```

**Runtime supervision:**

Two directions:
- **Handler death → Firecracker notified.** The example Rust handler uses `SO_PEERCRED` to get Firecracker's pid, then `kill(fc_pid, SIGKILL)` on panic. We should do the same: when the handler panics or exits abnormally, kill Firecracker to avoid a hung guest. Better than silent corruption.
- **Firecracker death → handler notified.** When Firecracker exits, its UFFD fd closes; reading from it returns EOF. Our handler sees EOF and exits cleanly.
- **Daemon death → everything orphaned.** That's existing mvm behavior; doesn't make UFFD specifically worse. systemd restart picks up state.

**Shutdown:**
- Normal `mvm stop`: daemon stops Firecracker gracefully. Handler sees UFFD fd closed, exits.
- Force kill: daemon sends SIGKILL to both. No corruption since memory is backed by the file anyway — on next restore we re-read from disk.

**Monitoring:**
- daemon polls handler liveness every 5s (does `kill -0 $uffdpid` equivalent). If dead unexpectedly, log loudly and attempt graceful VM shutdown.
- On restart, daemon rediscovers orphaned handlers from state and either adopts them (reattaches stderr pipe) or kills them.

### The handler must be a separate process (not a goroutine)

Why:
1. **Blocking syscall.** `poll()` on the UFFD fd in a goroutine would be fine, but any handler panic would crash the daemon, which kills all VMs.
2. **Binary separation.** `mvm-uffd` being a tiny static binary (one purpose) is easier to reason about, test, and upgrade independently.
3. **Supervision is simpler when it's a pid.** Matches the existing Firecracker-is-a-child-process pattern.

Estimated binary size: <5MB statically linked, smaller with `-ldflags="-s -w"`. Ships as part of the `mvm` distribution, goes into Lima alongside `firecracker`.

---

## 5. Expected Benchmarks

### Amazon's published numbers
From the Firecracker v1.1 release notes and subsequent docs: "2GB guest on GCE n2-standard-8, restore latency improved from ~7s to <100ms."

### Realistic expectations for our setup
Our current baseline is ~12s (slower than Amazon's 7s) because:
- Lima is running on macOS via Virtualization.framework → extra overhead
- The memory file may be on Lima's ext4 disk image (not native ZFS/btrfs)
- ARM64 on Apple Silicon — different memory subsystem characteristics

**Projected UFFD restore time for a 2GB VM:**
- Socket bind + mmap backing file (PROT_READ, MAP_POPULATE): ~10-50ms (MAP_POPULATE pre-faults the entire file — skip it if you want true lazy loading; use MAP_NORESERVE + plain MAP_PRIVATE instead).
- Firecracker API-only work (vCPU thread setup, device restore): ~20-40ms.
- First guest page faults (rolling, amortized, **does not block restore**).

**Target:** 30-80ms from "daemon starts mvm-uffd" to "resume_vm returned".

First-page-fault latency per page: dominated by one read from mmap'd file (served from page cache if MAP_POPULATE was used, ~1µs) + one UFFDIO_COPY ioctl (~2µs). Cold path from disk: ~100µs per page. For sparse workloads (guest touches <1% of memory in the first second), total wall-clock to "fully warm" is tens of ms.

The "28ms" figure cited in the user's brief is plausible for a small VM (256-512MB) and may not scale linearly — but <100ms for 2GB is well-documented.

### Claim: 400× speedup vs current
12,000ms → 30ms is a 400× speedup. It's real, demonstrated, and the architecture is proven at AWS scale (Lambda uses this for snapshots).

---

## 6. Risks and Gotchas

### High severity
1. **Handler crash = guest hang.** Covered above. Mitigation: supervise, and kill Firecracker on handler panic (Rust example does this; we must too).
2. **Path length limits.** Unix socket paths are capped at 108 bytes on Linux. `/var/lib/mvm/<vmname>/uffd.sock` with a long vm name can overflow. Mitigation: keep sockets under `/run/mvm/` with short names.
3. **Memory file offset correctness.** The `offset` field in the mapping is computed by Firecracker based on how it wrote the memory file. Trust it — do not compute your own offsets.

### Medium severity
4. **Sparse memory files.** Current `snapshot.go` uses `cp --sparse=always`. The memory file may have holes (guest never wrote those pages). When the handler reads from a hole, it gets zeros — which is correct. But: make sure we don't `MAP_POPULATE` huge sparse files unnecessarily.
5. **Page size assumption.** Firecracker sends `page_size` per region. We must not hardcode 4096. (We don't use hugepages, but future-proof.)
6. **EAGAIN on UFFDIO_COPY.** Happens when a REMOVE event is queued. Since we don't use balloon, this won't happen in practice, but handle it defensively: defer and retry.
7. **EEXIST on UFFDIO_COPY.** Happens when the same page is faulted concurrently by two vCPUs and both handlers try to populate. Benign — just continue.

### Low severity
8. **Kernel version drift.** The on-disk uffd_msg struct has been stable since Linux 4.11 and is versioned via UFFDIO_API handshake. We're fine on any kernel the Lima default template ships with (Ubuntu 24.04 → 6.8+).
9. **Debugging.** UFFD bugs manifest as frozen VMs. Add tracing: log every page fault + latency percentiles. Expose metrics endpoint (page faults served, EAGAIN count, p99 latency).
10. **Memory file deletion.** If someone deletes `mem.bin` while the handler is running, the mmap stays valid (the file is held open), but log loudly if we can detect it.

### Compatibility with existing code
- **No API client changes needed.** We're a daemon-side change; CLI is unaffected.
- **Encrypted snapshots.** Currently `snapshot.bin.enc` and `mem.bin.enc` are decrypted on restore. Need to decrypt to a temp file before `mmap()` — which we already do (line 109 in snapshot.go decrypts to plain path). Works as-is.
- **Pool warm VMs.** `internal/firecracker/pool.go` uses snapshot restore to warm slots. This code path gets UFFD-accelerated for free.

---

## 7. Open Questions / Not Yet Investigated

1. **MAP_POPULATE vs lazy mmap.** MAP_POPULATE pre-faults the entire backing file (~100ms for 2GB), eliminating disk I/O from the per-page critical path. Without it, cold page faults go to disk. For first-boot-after-daemon-start this might be slower. Benchmark both.
2. **Multiple memory regions.** If the VM has >3GB memory, Firecracker creates 2+ regions (MMIO gap). Our `findRegion` loop is O(n); for n<10 this is fine. No optimization needed.
3. **Hugepages.** We don't use them. If we ever do, `page_size` in mappings can be 2MB. Our handler is parametric on page_size, so it's handled — but UFFDIO_COPY with 2MB pages has additional flags. Revisit when/if we enable.
4. **seccomp.** Firecracker's seccomp filter allows the relevant syscalls. Our handler process needs its own minimal seccomp policy: mmap, read, ioctl, poll, recvmsg, close, exit. Straightforward but must be done.
5. **Memory file encryption-at-rest during restore.** Current flow decrypts to disk first. For true at-rest security we'd want to mmap the encrypted file and decrypt on page-fault. Out of scope for this doc.

---

## 8. Implementation Plan (suggested, for the follow-up task)

1. **Write `internal/uffd/uffd.go`:** syscall wrappers, constants, structs. Unit-test struct layouts with a fake uffd fd. (~1 day.)
2. **Write `internal/uffd/handler.go`:** the event loop. Integration test with a fake Firecracker that sends JSON+fd via SCM_RIGHTS. (~1 day.)
3. **Write `cmd/mvm-uffd/main.go`:** tiny wrapper. Add to `build.go` and `install.go` to ship into Lima. (~0.5 day.)
4. **Modify `snapshot.go:loadSnapshot`:** start mvm-uffd, wait for socket, flip `backend_type` to `Uffd`. Keep a `USE_UFFD` env flag for quick rollback. (~0.5 day.)
5. **Add supervision to daemon:** track UFFDPid per VM, monitor liveness, kill on VM stop. (~1 day.)
6. **Benchmark on Apple Silicon Lima:** cold restore, warm restore, 512MB / 1GB / 2GB VMs. Target: <100ms wall-clock. (~0.5 day.)
7. **Total:** ~4-5 engineer days.

---

## Appendix A: Reference Files

- Firecracker handler (Rust, authoritative wire format): https://github.com/firecracker-microvm/firecracker/blob/main/src/firecracker/examples/uffd/on_demand_handler.rs
- Firecracker UFFD utils (Rust, message layout & logic): https://github.com/firecracker-microvm/firecracker/blob/main/src/firecracker/examples/uffd/uffd_utils.rs
- Firecracker UFFD docs: https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md
- Linux userfaultfd docs: https://www.kernel.org/doc/html/latest/admin-guide/mm/userfaultfd.html
- Snapshot load API: https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md
- Firecracker CHANGELOG (UFFD history): https://github.com/firecracker-microvm/firecracker/blob/main/CHANGELOG.md
- Local copies fetched during research:
  - `/tmp/on_demand_handler.rs`
  - `/tmp/uffd_utils.rs`
  - `/tmp/uffd-docs.md`

## Appendix B: Current Firecracker Version

- Pinned at `v1.13.0` in `/Users/paulmeller/Projects/firecracker/internal/firecracker/install.go:20`.
- UFFD supported since `v1.1.0`. No version bump required.

## Appendix C: Files That Will Need Changes

- `/Users/paulmeller/Projects/firecracker/internal/firecracker/snapshot.go` (lines 142-194) — flip backend_type, start handler
- `/Users/paulmeller/Projects/firecracker/internal/firecracker/pool.go` — pool warm path uses the same restore (gets the speedup for free)
- `/Users/paulmeller/Projects/firecracker/internal/firecracker/install.go` — bundle mvm-uffd binary
- `/Users/paulmeller/Projects/firecracker/internal/firecracker/build.go` — cross-compile mvm-uffd for linux/arm64
- `/Users/paulmeller/Projects/firecracker/internal/state/store.go` — track UFFDPid per VM
- `/Users/paulmeller/Projects/firecracker/internal/uffd/` — NEW package
- `/Users/paulmeller/Projects/firecracker/cmd/mvm-uffd/` — NEW binary
