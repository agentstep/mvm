# mvm

### Hardware-isolated microVMs for AI agents. One binary, your Mac or your servers.

Give every AI agent its own real Linux machine — root, shell, network, the works — with hardware isolation, not a shared kernel. The **same `mvm` binary** runs natively on Apple Silicon and on the bare-metal Linux servers you already own. Same CLI, same SDKs, same API. No cloud vendor, no per-second meter, no code leaving your machine.

```bash
mvm start sandbox                          # ~0.7s to a booted, isolated Linux VM
mvm exec sandbox -- claude --dangerously-skip-permissions
mvm snapshot create sandbox                # checkpoint memory + disk
mvm stop sandbox && mvm start sandbox      # restore in ~0.3s, exactly where you left off
```

[Get started](#get-started) · [Benchmarks](#benchmarks) · [How it works](#how-it-works) · [GitHub](https://github.com/agentstep/mvm)

---

## The problem

AI coding agents need root, a real shell, and network access to do useful work. Today you choose between bad options:

- **Docker** — shared kernel, namespace isolation. One CVE away from a container escape on the host running your agent.
- **Hosted sandboxes** (E2B, Daytona, Fly Sprites, Cloudflare) — real isolation, but your code and credentials leave your machine, you pay a per-second meter, and every tool call pays a network round-trip.
- **Local-only sandboxes** — fine on a laptop, but no path to running the same thing on servers you control.

Nobody offered *"the same hardware-isolated sandbox, locally on my Mac **and** self-hosted on my servers, with one API."*

**mvm does.**

---

## What you get

### 🔒 Real hardware isolation, not namespaces
Each sandbox is its own microVM with its own kernel. On Linux servers that's **Firecracker + KVM** — the same hypervisor behind AWS Lambda. On macOS it's **Apple's Virtualization.framework**, running *natively* on Apple Silicon with no nesting tax. Not a shared kernel (Docker), not a re-implemented userspace kernel (gVisor) — a real hardware boundary on both platforms.

### ⚡ Native macOS speed
The Apple VZ backend boots a fresh, agent-ready VM in **~0.7s** and restores a checkpointed one in **~0.3s** — measured, not aspirational ([benchmarks below](#benchmarks)). No Linux-VM-inside-your-Mac penalty, because there is no nested layer. Apple validated this exact model at WWDC25 with its own [`container`](https://github.com/apple/container) tool (one lightweight VM per workload on Apple Silicon); mvm takes that isolation and builds the agent runtime — pool, exec, checkpoint, self-hosted Firecracker on Linux — that Apple's developer tool doesn't try to be.

### ⏪ Checkpoint and undo — on your machine, not someone's cloud
`mvm snapshot create` freezes both **memory and disk**. Restore drops you back exactly where you were — a running process keeps running, and any file written *after* the checkpoint is gone. It's the same "stateful sandbox with checkpoint & restore" pitch hosted services like Fly Sprites charge for — except mvm restores in **~0.29s** (verified) and runs on hardware you control, not theirs. No cloud account, no per-second meter, no code leaving your machine.

### 🖥️ One binary, two homes
Develop against a sandbox on your Mac. Deploy the identical binary to a bare-metal Linux box. Your CLI commands, SDK calls, and API don't change.

### 🛠️ Built for agents, not humans
- **16ms warm exec** over vsock — no SSH, no TCP round-trip. Agents firing dozens of sequential tool calls save seconds per session.
- **Interactive PTY** (`mvm exec -it -- bash`) over a hijacked connection — full terminal, pure Go, no SSH daemon in the guest.
- **Warm pool** for ~1s claimed starts.
- **Per-VM network policy** (`--net-policy deny` / `allow:domains`) and seccomp profiles.

### 🏠 Yours, end to end
Apache 2.0, open source. Self-host on hardware you control — air-gapped, regulated, or sovereign-data environments included. Code and credentials never leave your infrastructure. No per-second billing.

---

## Benchmarks

**Apple VZ backend, native on Apple Silicon** — measured 2026-06-21, every sample verified *running + agent-responding* before counting (n=6 cold, n=8 restore, zero failures):

| Operation | Median | Range |
|-----------|--------|-------|
| **Cold boot** (fresh VM → agent ready) | **0.697s** | 0.660–0.718s |
| **Fast-restore** (from memory+disk checkpoint) | **0.293s**¹ | 0.289–0.353s |
| **Disk rollback** on restore | ✅ works¹ | post-checkpoint writes vanish |

> ¹ Verified on macOS ≤26.1. A macOS 26.2 change currently blocks restore on the ad-hoc-signed helper (`VZError 12`); fix in progress (Developer-ID signing). Cold boot and pause/resume are unaffected.
| **Warm exec** (host → guest agent over vsock) | **16ms** | — |

Why this matters: Firecracker **cannot run natively on macOS** — it needs a Linux host, which on a Mac means a nested VM and a real performance tax. mvm sidesteps that entirely on the desktop by using Apple's own hypervisor, then gives you Firecracker + KVM when you deploy to Linux.

> Honest caveat: these are single-machine numbers on Apple Silicon. We haven't yet published cross-machine or in-guest I/O (fio) figures — those are tracked as open work, not claimed here.

---

## How it works

**On macOS — native, no nesting:**
```
macOS CLI ──> Apple Virtualization.framework ──> microVM (own kernel)
                       │
                  vsock control plane ──> in-guest agent (exec, PTY, net setup)
```

**On a Linux server you own:**
```
CLI / SDK ──HTTPS+API key──> mvm daemon ──> Firecracker + KVM microVMs
```

It's the same binary in both places. The daemon is the single source of truth for VM state; the macOS CLI is a thin client. The in-guest **agent** speaks a vsock protocol for exec, interactive PTY, and network configuration — no SSH anywhere in the stack.

---

## How it compares

|  | mvm | microsandbox | E2B | Fly Sprites | Daytona | Modal |
|---|---|---|---|---|---|---|
| **Isolation** | Apple VZ + Firecracker/KVM | libkrun (KVM/HVF) | Firecracker | Firecracker | Docker¹ | gVisor² |
| **Hardware-isolated microVM** | ✅ both platforms | ✅ | ✅ | ✅ | opt-in only¹ | ❌² |
| **Runs natively on macOS** | ✅ | ✅ | ❌ | ❌ | ✅ (Docker) | ❌ |
| **Self-host on your servers** | ✅ | ✅ | ✅ (BYOC) | ❌ cloud-only | ✅ | ❌ |
| **Open source** | ✅ Apache 2.0 | ✅ Apache 2.0 | ✅ Apache 2.0 | ❌ | ✅ AGPL-3.0 | ❌ |
| **Checkpoint + disk undo** | ✅ local, ~0.29s | — | — | ✅ cloud, ~300ms³ | ❌ | mem-snapshot |
| **Code stays on your infra** | ✅ | ✅ | self-host only | ❌ | ✅ | ❌ |

<sub>¹ Daytona defaults to Docker containers (shared kernel); microVM isolation requires opting into Kata. ² Modal uses gVisor — userspace syscall interception, not a hardware-virtualized VM. ³ Sprites' ~300ms is a published marketing figure; it is cloud-only and closed-source (a local OSS version is announced but unshipped as of mid-2026). Competitor facts gathered June 2026 from vendors' own sites/docs; verify before publishing.</sub>

**The wedge:** every Firecracker-grade competitor is cloud-first (E2B, Sprites, Vercel) or closed (Sprites, Modal, Northflank, Cloudflare). The one that's genuinely local-and-open (Daytona) defaults to plain Docker, not a microVM. **microsandbox is the only true overlap** — and against it, mvm's edge is first-party engines per OS (Apple's own hypervisor on the Mac, battle-tested Firecracker on Linux) instead of one library VMM, shipped as **a single binary**.

**mvm is the only hardware-isolated sandbox that runs natively on your Mac *and* self-hosts on your servers behind one API.**

---

## Get started

**macOS (Apple Silicon, macOS 15+):**
```bash
brew install agentstep/tap/mvm     # or build from source
mvm start sandbox
mvm exec sandbox -- echo "hello from an isolated VM"
```

**Self-host on a Linux box with `/dev/kvm`:**
```bash
curl -sSL https://get.mvm.dev | sudo bash      # fresh box → working service in ~95s
```
Then, from any laptop or CI:
```bash
export MVM_REMOTE=https://your-server:19876
mvm exec sandbox -- npm test                   # same CLI, same API
```

**Or from code** — same API, local or remote:
```python
pip install mvm-sandbox
```
```bash
npm install @agentstep/mvm-sdk
go get github.com/agentstep/mvm/sdk
```

---

## Requirements

| | Local (macOS) | Self-hosted (Linux) |
|---|---|---|
| Hardware | Apple Silicon (M3+) | Bare metal with `/dev/kvm` |
| OS | macOS 15 (Sequoia)+ | Any modern Linux |
| Works on | your laptop | AWS `.metal`, Hetzner/OVH dedicated, GCP nested-virt, on-prem |
| Does **not** work on | — | standard cloud VMs without nested virt (EC2 t3, GCP e2) |

---

## FAQ

**Is this a hosted service?**
No — that's the point. mvm is the self-hosted / local alternative to hosted sandbox providers. You run it on your own Mac or your own servers.

**Why two hypervisors?**
Firecracker is the gold standard on Linux but can't boot natively on macOS. Apple's Virtualization.framework gives Mac users native-speed microVMs with no nested-VM tax. mvm picks the right one for the platform and hides the difference behind one API.

**How is this different from Docker?**
Docker containers share the host kernel. A microVM has its own kernel and is isolated in hardware by the hypervisor — the same boundary AWS Lambda relies on. For running untrusted agent code with root, that boundary matters.

**How is this different from E2B, Fly Sprites, or Modal?**
E2B and Sprites use Firecracker too, but they're cloud-first — your code runs on their infrastructure and you pay a per-second meter (Sprites is also closed-source and cloud-only). Modal uses gVisor, which is userspace syscall filtering, not a hardware-virtualized VM. mvm gives you the same microVM-grade isolation but running on *your* Mac for development and *your* servers in production, behind one API — nothing leaves your infrastructure.

**What about microsandbox?**
Closest open-source cousin — it also runs microVMs locally and self-hosted. The difference is what's underneath: microsandbox uses the libkrun library VMM on both platforms, while mvm uses Apple's first-party Virtualization.framework on macOS and battle-tested Firecracker on Linux, shipped as a single binary.

**Is it really open source?**
Yes, Apache 2.0, at [github.com/agentstep/mvm](https://github.com/agentstep/mvm).

---

<div align="center">

**Give your agents a real machine — without giving up your own.**

[Get started](#get-started) · [GitHub](https://github.com/agentstep/mvm) · Apache 2.0

</div>
