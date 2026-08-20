# Inner Container — Design (sub-project A)

**Status:** design approved in conversation 2026-08-20; reviewed adversarially by a
Fable architect pass whose corrections are folded in below.

**Goal:** Slide a namespace boundary between user code and the guest kernel, so
that `mvm-agent` supervises user processes from outside the namespace they run
in. This is the prerequisite for a service manager that survives a restart of
user code (sub-project C) and for a bounce primitive (sub-project B).

**Non-goal: security isolation.** The guest runs as root, there is no user
namespace, the network namespace is shared, the rootfs is shared, and the
control socket is reachable. This container is a **lifecycle boundary and
nothing else**. No document, comment, or marketing copy may imply otherwise.

**Success criterion:** parity. `exec`, `exec -it` (PTY), streaming exec, file
read/write, volume mounts, secret injection, stop, pause/resume and
snapshot/restore all behave as they do today, on both backends.

---

## 1. Why this shape

Fly's Sprites put their orchestration in the root namespace and user code in an
inner container, which is what lets them restart user code without rebooting the
VM. mvm today has the opposite arrangement: `mvm-agent` is PID 1 in the same
namespace as everything it supervises.

**Honest assessment of the return.** `handleStartVM` already performs a
disk-preserving cold reboot (`internal/server/routes.go:420-423`), applevz
persists MAC and machine-id so the address survives, and boot is ~0.7s. A VM
reboot therefore already delivers most of what a bounce would, plus a kernel and
netns reset. The inner container's net gain is narrow: process reset **without
dropping host-side vsock connections and port forwards**, and a place for
sub-project C's supervisor to live that survives that reset.

Two independent reviews recommended against building it on that basis. It is
being built anyway, as an explicit decision. This document records the reasoning
so a future reader does not mistake it for an oversight.

## 2. Prerequisite defects

These are live bugs today, found while mapping the design. They are fixed
**first**, because A either depends on them or makes them worse.

| # | Defect | Why it blocks A |
|---|---|---|
| P1 | **The agent never reaps.** No `SIGCHLD`, `wait4`, or `signal.Notify` anywhere in `agent/`. mvm-agent is PID 1, so every `mvm exec -d` orphan zombifies permanently. | A introduces an inner PID 1 that *must* reap. Doing so naively corrupts exit codes (see §5). |
| P2 | **Exit codes report 0 on failure.** `exec_pty.go:191` and `exec_stream.go:66` assign `exitCode` only inside the `*exec.ExitError` branch; any other `Wait()` error leaves it at zero. | A reaper makes `Wait()` return `ECHILD` — not an `ExitError` — so this fires constantly instead of rarely. |
| P3 | **Two accept loops race on the same TCP listener.** `main.go:33` applies the cross-VM gateway filter; `main.go:86` accepts the same `tcpLn` (via `ln = tcpLn`) with no filter. Roughly half of connections bypass the protection described at `main.go:25-31`. | A restructures this exact dispatch. Carrying the bug across the refactor would bake it in. |
| P4 | **`poweroff` is dead code.** `exec.Command("poweroff").Start()`, commented "works on Alpine", on a Debian rootfs with no systemd or sysvinit; the error is discarded. `StopViaAgent`'s `kill -9` fallback masks it. | §7 keeps poweroff in the outer namespace; porting a dead path across a boundary is worse than fixing it. |

## 3. Process topology

```
guest kernel
└─ mvm-agent  (PID 1, root namespace)
     │  vsock + TCP listeners        — accepts, reads first frame, routes
     │  reaper: wait4(-1) → status registry
     │  mount verb (outer-owned)
     │  socketpair(AF_UNIX)  ── control channel only
     └─ /proc/self/exe --container-init   (PID 1, inner namespace)
            │  reaper: wait4(-1) → status registry
            │  receives conn fds via SCM_RIGHTS
            ├─ session: bash        (exec_pty)
            └─ session: npm test    (exec_stream)
```

**Namespaces:** `CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWIPC | CLONE_NEWUTS`.

Verified working on the live guest kernel — `unshare --pid --mount --ipc --uts
--fork --mount-proc` yields `inner-pid=1` and 4 visible processes versus 60
outside. Note this proves *kernel support*, not the Go sequence: `unshare(1)`
silently does the make-private and `/proc` remount that our inner init must
perform itself.

**Network namespace is deliberately NOT unshared.** Sharing it preserves the
guest IP, the iptables DNAT port forwarding, the `tcp_forward` handler, and
in-guest network policy without modification — which matters when the success
criterion is parity. It costs nothing in isolation terms, since the control
channel is vsock (not IP), and egress policy is moving host-side under a
separate plan.

**No `CLONE_NEWUSER`.** The guest is root-only by design; a user namespace is
the most likely to be restricted and would complicate every uid-sensitive path
(`exec -u`, volume ownership) for no gain.

**Re-exec via `/proc/self/exe`, not `/opt/mvm-agent`.** The magic link pins the
running inode, so a binary upgrade or a respawn after crash cannot produce a
protocol-skewed inner init.

## 4. Transport: control channel plus fd passing

This is the crux, and the naive design is wrong.

The socketpair **cannot** multiplex sessions. `exec_pty` takes over the
connection for the session's lifetime (`main.go:132`) and `tcp_forward` switches
it to a raw unframed `io.Copy` relay (`tcp_forward.go:41-44`). Carrying those
over one shared pipe would require inventing channel multiplexing — new framing,
per-channel flow control, and head-of-line blocking on interactive keystrokes.

**Instead:** the socketpair carries control messages only (spawn, handshake,
health, mount). Connections are handed over whole.

```
outer: conn := ln.Accept()
outer: protocol.ReadFrame(conn, &req)        // routing needs the verb
outer: switch route(req.Type) {
         case outerHandled: ...existing code, unchanged...
         case innerHandled: sendFD(ctrl, connFD, req)   // SCM_RIGHTS
       }
inner: fd, req := recvFD(ctrl)
inner: go handleConnection(net.FileConn(fd), withPushedBackRequest(req))
```

The already-read request frame travels alongside the fd, and the inner side
resumes the existing `handleConnection` loop with that frame pushed back — so
handler code is reused verbatim rather than rewritten.

**This dissolves the PTY problem.** `openPty` (`exec_pty.go:23-48`) opens
`/dev/ptmx` and `/dev/pts/N` *in the same process that spawns the child*. With
fd passing, that process is the inner init, so master and slave both come from
the inner devpts instance, `Setctty`/`Ctty: 0` work unchanged, and frames are
written straight to the passed fd. PTY is only high-risk under the multiplexing
design; here it is low-risk.

**Two fd-hygiene requirements, both mandatory:**

1. **Re-set `FD_CLOEXEC`.** Go's `ExtraFiles` clears CLOEXEC on inherited fds,
   and `SCM_RIGHTS`-received fds arrive without it. If the inner init spawns a
   session before re-setting it, **every user process inherits the control
   channel** — breaking EOF-based death detection and letting sandboxed code
   write frames to the outer agent.
2. **Keep the `os.File` alive.** A garbage-collected `os.File`'s finalizer
   closes the underlying fd, producing unreproducible connection drops. Hold a
   reference for the fd's whole lifetime.

**On `SysProcAttr.Cloneflags` from multithreaded Go:** safe. The flags apply to
the cloned child, and Go's fork path runs only async-signal-safe code before
exec. Avoiding `setns` was correct — it is per-thread, and the Go scheduler
migrates goroutines across threads.

## 5. Reaping and exit-code correctness

A PID 1 must reap orphans. But a naive `wait4(-1)` loop running alongside
`os/exec` **steals session exit statuses**: the reaper collects the status
first, `cmd.Wait()` then fails with `ECHILD`, and because of P2 the handler
reports **exit code 0 for a failed command**. The failure is load-dependent —
it passes casual testing and fails under concurrency.

**Design:** a single owner of `wait4(-1)` results per namespace.

```
reaper goroutine:
    for {
        pid, status := syscall.Wait4(-1, &status, 0, nil)   // blocking, no WNOHANG
        if ch, ok := registry.claim(pid); ok { ch <- status }   // a session's child
        // else: an orphan; already reaped, nothing more to do
    }
```

Session spawn registers its pid before waiting; the handler waits on its channel
rather than calling `cmd.Wait()` directly, and falls back to the registry when
`Wait()` returns `ECHILD`. P2 is fixed as part of this: any non-`ExitError`
error must produce a distinct non-zero code, never 0.

**Rejected:** pausing the reaper while a session runs. A single long-lived
`mvm exec -it bash` would let zombies accumulate without bound.

## 6. Mount propagation

Default `CLONE_NEWNS` from a private parent gives private propagation in both
directions, which breaks applevz volumes.

applevz mounts volumes **post-boot via opaque agent exec**: `virtiofsMountCommands`
builds `mkdir -p X && mount -t virtiofs tagN X` and `runStartAppleVZ` runs it
through `agent.Exec` (`internal/cli/start.go:374-383`, `676-682`). Once exec
moves inside, those mounts exist only in the inner namespace, as shell text the
outer agent cannot see. A bounce or an inner-init crash then yields a fresh
namespace with **empty mount points and no error** — silent data-invisibility.

**Design:**

- `mvm-init` runs `mount --make-rshared /` before exec'ing the agent.
- Inner init runs `mount --make-rslave /` after clone, then mounts its own
  `/proc`, `devpts`, and `/dev/shm`.
- A new **`mount` request verb**, handled by the **outer** agent, replaces the
  opaque shell mount. applevz virtiofs setup moves onto it.

Outer-namespace mounts then propagate into every current and future inner
namespace, survive a bounce by construction, and stop being untracked shell
strings.

Firecracker volumes are unaffected: tar copy-in is plain file writes on the
shared superblock (`internal/firecracker/security.go:54-100`), which propagate
regardless of mount namespace.

**`/dev/shm` sizing.** `mvm-init` currently mounts it at `size=50%`
(`internal/cli/init.go:432`). A per-container instance at another 50% lets the
two jointly reach 100% of guest RAM, defeating the cap. The inner instance is
capped so the two sum below RAM; the outer one stays for root-namespace
services in sub-project C.

## 7. Handler routing

"Everything moves inside" is wrong. Four handlers move; the rest must not.

| Handler | Side | Reason |
|---|---|---|
| `exec` | **inner** | user code |
| `exec_stream` | **inner** | user code |
| `exec_pty` | **inner** | user code; needs the inner devpts instance |
| `file` | **inner** | must resolve inner-only mounts (`/dev/shm`, inner volumes) |
| `poweroff` | **outer** | `reboot(2)` in a non-initial PID namespace terminates the *namespace*, not the machine — `mvm stop`'s graceful path would report success while only killing the container. Fixed per P4 as `sync()` + `reboot(RB_POWER_OFF)`. |
| `tcp_forward` | **outer** | dials `127.0.0.1` (`tcp_forward.go:26-27`); netns is shared so it reaches inner services identically — and keeping it outer removes the raw-relay case from the transport entirely |
| `setup_network`, `net_info` | **outer** | netlink and `/proc/net` are netns-scoped; netns is shared, so behaviour is identical either side |
| `ping` | **outer** | liveness of the agent, not of user code |

`exec -u` (`su -` wrapping, `cli/exec.go:346-348`) and per-exec secret injection
are script-side and move with exec. A shared `/etc/passwd` makes them
namespace-agnostic, but both stay in the parity matrix.

## 8. Inner-init lifecycle

When a PID-namespace's PID 1 dies, the kernel SIGKILLs every process in it and
the namespace is permanently unusable — nothing can be spawned into it again.

The outer agent therefore must:

1. **Detect death** — `Wait()` on the inner init, plus socketpair EOF (which is
   only reliable if the CLOEXEC hygiene in §4 holds; a lingering user fd would
   mask it).
2. **Respawn a fresh namespace** and replay per-container setup: private
   `/proc`, `devpts`, `/dev/shm`.
3. **Handle in-flight requests** in the gap: fail them with a distinct,
   identifiable error rather than hanging. In-flight sessions are lost; that is
   inherent and must be documented.

**This respawn path is sub-project B's bounce primitive.** Building it correctly
in A means B becomes "expose the existing path as a verb" rather than new
machinery — which is the strongest standalone argument for A.

**Signals.** Inner init installs `signal.Notify` for `SIGCHLD` only. Signals
from inside the namespace to its PID 1 are dropped unless a handler exists
(including SIGKILL — the kernel blocks it, which is desirable). Deliberately
**no SIGTERM handler**: with one, a user's stray `kill 1` inside the sandbox
would trigger container suicide. Lifecycle is driven exclusively over the
control channel. `kill -TERM 1` inside the guest becomes a no-op, which is what
it already is today.

## 9. What observably changes

A is *not* invisible. Documented deliberately:

- `ps aux` inside a session shows the container's processes, not the guest's.
- `/proc/1` is the inner init, not mvm-agent.
- `mvm exec -d` orphans are now **reaped** instead of zombifying (a fix, but a
  behavioural diff from today).
- Sessions die if the inner init dies.
- `/proc` contents users actually read (`meminfo`, `cpuinfo`, `loadavg`) are
  global rather than namespaced, so in-guest tooling reports the same numbers.

## 10. Bounce contract (written now, consumed by B)

Recording this in A prevents B shipping with misleading semantics.

**Resets:** processes, PTYs, IPC objects, `/dev/shm` contents, inner mounts,
hostname.

**Persists:** every file (including `/tmp`), iptables rules, routes, network
sysctls, TIME_WAIT sockets, root-namespace services.

Because the netns is shared, **a bounce cannot clear wedged in-guest network
state**. User code runs as root and can iptables itself into a corner. This is
acceptable only because egress policy is moving host-side — so **B is explicitly
dependent on that plan landing**, or it ships with a known "cannot un-break
networking" hole.

The shared-rootfs choice is right for the goal (writes must survive a bounce, so
an overlay is disqualified). The risk is messaging: "reset" implies more than it
delivers, hence this table.

## 11. Testing

Unit-testable without a VM: routing table (`route(reqType)`), the status
registry under concurrent claim/reap, fd-hygiene helpers, mount-verb
construction, inner-init argv construction.

**Cross-backend parity suite** — the real gate, run on Firecracker and applevz:

- exit codes: success, failure, signal death, **and failure under a
  concurrent-orphan storm** (the P1/P2 regression)
- `exec -d` detach; assert no zombie accumulates
- `exec -u`, `-w`, env and secret injection
- stdin piping; `exec -it` PTY including resize
- file read/write against an inner-only mount
- volume mounts on both backends, **including after an inner-init respawn**
- `mvm stop` graceful path (P4)
- pause/resume, snapshot/restore
- cross-VM TCP rejection still enforced (P3)

**Non-findings, verified:** snapshot/restore and pause/resume are whole-VM
operations; nested namespaces live in guest memory and restore transparently.
The Firecracker resume path re-runs volume copy-in via exec (`routes.go:239-264`),
which is shared-superblock file writes. Host-side stats read
`/proc/<vmm-pid>` and are unaffected. The seccomp `strict` profile's
`mount -o remount,ro /` (`security.go:24`) is a superblock-level remount and
still applies globally from inside — parity holds, but the suite asserts it.

## 12. Implementation status (2026-08-20)

| Step | State | Evidence |
|---|---|---|
| 0. Prerequisite defects | **done** | Reaper verified live: 5 zombies without it, 0 with it, as real PID 1 |
| 1. Dark launch | **done** | Namespace inodes differ for pid/mnt/ipc/uts, shared for net; kill -9 respawned |
| 2. Mount propagation | **done** | Outer tmpfs mounted *after* container start is visible inside, and survives respawn |
| 3. fd passing + routing | **done** | Routed exec reports the inner pid/mnt namespaces, `pid=10` |
| 4. Route stream/file/pty | **code complete** | PTY verified: inner ns, `tty` → `/dev/pts/0`, exit code 7 propagated |
| 5. Flip the default | **blocked** | See below |

Routing is gated behind `MVM_CONTAINER_EXEC`. Unset — the default — the
container is created and supervised but nothing is routed to it, so behaviour is
byte-identical to before.

**What blocks step 5.** Flipping the default requires the cross-backend parity
suite in §11 to pass, and that requires the new agent to *be* the guest's agent
— which means rebuilding `base.ext4`. The rootfs build path
(`buildRootfsViaDocker`) needs Docker, which is not installed on this machine.
Every verification above was therefore done by mounting the new binary into a
guest via `-V` and running it as PID 1 inside a nested namespace. That exercises
the real code paths, but it is not the same as the agent booting as the guest's
own PID 1.

Specifically unverified until then: parity for the four routed handlers as
invoked by the real `mvm exec` host path, volume behaviour after an inner-init
respawn on both backends, `exec -u`, stdin piping, PTY resize, and the
stop/pause/snapshot lifecycle with a container running.

## 13. Rollout

Not a big-bang refactor. Each step is shippable and parity-diffable.

0. **Prerequisite defects** — P1 reaper design, P2 exit codes, P3 accept-loop
   race, P4 poweroff. Independently valuable; all four are live bugs.
1. **Dark launch** — inner-init spawn (Cloneflags, `/proc/self/exe`, socketpair
   handshake, SIGCHLD respawn, status registry) with **nothing routed to it**.
   Zero behaviour change; soak it.
2. **Mounts** — `--make-rshared` in mvm-init, `--make-rslave` in inner, the
   outer-handled `mount` verb, applevz virtiofs migrated onto it.
3. **Route `exec`** via fd passing behind a flag, old path as fallback. Build
   the parity suite here.
4. **Route `exec_stream`, then `file`, then `exec_pty`.** Net handlers,
   `tcp_forward` and `poweroff` stay outer permanently.
5. **Flip the default, delete the fallback.**
