# mvm — Product Brief

## One-liner

Firecracker microVMs for AI agents. Same binary runs on your Mac and on your servers. Free and open source.

## Problem

AI coding agents (Claude Code, Codex, Gemini, LangChain, OpenAI Agents SDK) need root, shell, and network access to do real work. The existing options:

- **Docker** — namespace isolation, shared kernel, one CVE from container escape
- **Hosted sandboxes** — E2B, Daytona, Sprites, Cloudflare Sandbox give real isolation but your code and credentials leave your machine. Per-second billing. Network-bound latency.
- **Local-only sandboxes** — microsandbox works on your laptop but has no cloud deployment story

Nobody offers **"same Firecracker sandbox API locally AND on your own servers."** mvm does.

## Solution

mvm gives each AI agent its own Firecracker microVM with KVM hardware isolation — the same technology behind AWS Lambda. Use the same CLI and SDKs locally on macOS or on a bare-metal Linux server you control.

### Two deployment modes, same product

**Local mode** (macOS via Lima):
```bash
mvm start sandbox
mvm exec sandbox -- claude --dangerously-skip-permissions
```

**Cloud mode** (bare-metal Linux with KVM):
```bash
curl -sSL https://get.mvm.dev | sudo bash    # 95s: fresh box → working service
```

From any laptop or CI:
```bash
export MVM_REMOTE=https://server:19876
mvm exec sandbox -- any-command          # same CLI, same API
```

Or programmatically via Python / TypeScript / Go SDKs.

## Key metrics (April 2026, GCP n2-standard-4)

| Metric | mvm | Best competitor |
|--------|-----|-----------------|
| **Exec latency (local daemon)** | **16ms** | E2B/Daytona 50-200ms (network) |
| **TTI (create + first exec)** | 1.7s | Daytona 120ms |
| **VM start from pool** | 1.1-1.4s | — |
| **Install from scratch** | **95s** | N/A (all hosted) |
| **Snapshot restore (UFFD)** | ~30-100ms target | Stock FC 28ms |
| **Cost (sustained 24/7)** | **$0 + server cost** | Sprites $655/mo |
| **Isolation** | Firecracker+KVM | Same (E2B/Sprites), weaker (Daytona/CF) |

## How it works

### Local mode
```
macOS → Lima VM (nested virt) → Firecracker microVMs (KVM)
```

Lima provides a Linux environment with nested virtualization. A daemon runs inside Lima. The macOS CLI talks to it over a forwarded Unix socket.

### Cloud mode
```
Client (CLI / SDK) → TCP+TLS → daemon (bare-metal Linux) → Firecracker microVMs
```

Same daemon binary. HTTPS with API key auth. No cloud vendor.

### Features working today

**Core lifecycle**
- `start`, `stop`, `exec`, `pause`, `resume`, `list`, `delete`
- Interactive TTY exec via HTTP connection hijack (no SSH)
- Warm pool for 1-second starts
- Self-documenting VMs (`/.mvm/SKILLS.md`)

**Security**
- Per-VM network policies (`--net-policy deny` or `allow:domains`)
- Seccomp profiles (`--seccomp strict`)
- KVM hardware isolation
- API key auth on cloud mode
- Filesystem diff tracking

**Snapshots & checkpoints**
- Full snapshots with rootfs + memory
- **UFFD-based lazy restore** (page-in on demand, target <100ms)
- AES-256-GCM encryption option
- Restore to new VM from any snapshot

**Custom images**
- `mvm build -f Dockerfile -t my-image` — build custom rootfs without Docker
- `mvm start sandbox --image my-image`

**SDKs** — local and remote, same API
- Python (`pip install mvm-sandbox`)
- TypeScript (`npm install @agentstep/mvm-sdk`)
- Go (`go get github.com/agentstep/mvm/sdk`)

**Deployment**
- `curl | bash` install script for Linux servers
- Systemd unit with self-signed TLS + random API key
- Auto-generates directories, NAT rules, and kernel+rootfs

## Competitive landscape

| Product | Isolation | Local dev | Self-host | Open source | Per-call latency |
|---------|-----------|-----------|-----------|-------------|-------------------|
| **mvm** | Firecracker+KVM | **Yes** | **Yes** | **Yes** | **16ms local / 100ms cloud** |
| E2B | Firecracker | No | No | Partial | 50-200ms |
| Daytona | Docker | No | Enterprise | No | 50-200ms |
| Sprites | Firecracker | No | No | No | 50-200ms |
| Cloudflare Sandbox | Container | No | No | No | 50-200ms |
| microsandbox | libkrun+KVM | Yes | Local only | Yes | ~local |
| Docker Sandboxes | microVM | Yes | Yes | Proprietary | — |

**mvm is the only Firecracker+KVM product that runs local AND self-hosted with the same API.**

## Target users

**Developer on a Mac** — Runs Claude Code / Codex / Cursor with full root access knowing the agent can't touch the host. Uses pool for instant starts, pause for safety before risky ops.

**Platform team at a company** — Deploys mvm on bare-metal Linux (Hetzner dedicated, AWS `.metal`, on-prem) so internal developers and agents get sandboxes without sending code to a third-party cloud. Compliance-friendly.

**AI agent framework builder** — Uses the Python/TS SDK to give their framework's users sandboxes, whether running on their laptop or on shared infrastructure. Users never know the difference.

## Unique value propositions

1. **Same API local + cloud** — dev on your Mac, deploy to your servers, no code changes. No other product in this space offers this.

2. **16ms local exec** — agents doing many sequential tool calls save 2-10s per session vs any hosted provider.

3. **Free for self-hosting** — 10-15× cheaper than Sprites/E2B for sustained workloads. No per-second meter.

4. **Compliance-ready** — code never leaves your infrastructure. No cloud vendor requirement. Suitable for air-gapped, regulated, or sovereign-data environments.

5. **Firecracker-grade isolation** — same hypervisor as AWS Lambda. Stronger than Docker-based competitors (Daytona), stronger than container-based (Cloudflare Sandbox).

## Requirements

### Local (macOS)
- Apple Silicon M3+
- macOS 15 (Sequoia) or newer

### Cloud (bare-metal Linux)
- `/dev/kvm` accessible
- Supported: AWS `.metal`, GCP with nested virt, Hetzner/OVH dedicated, any bare-metal
- NOT supported: standard cloud VMs (EC2 t3, GCP e2) without nested virt

## Architecture

```
cmd/
├── mvm/             Single binary: CLI + daemon
├── mvm-agent/       In-guest agent (vsock protocol)
└── mvm-uffd/        UFFD page-fault handler

internal/
├── cli/             Cobra CLI (local and remote)
├── server/          HTTP daemon (Unix socket + TCP+TLS)
├── firecracker/     FC lifecycle, pool, snapshots, build
├── agentclient/     Host-side client for in-guest agent (vsock)
├── uffd/            Userfaultfd handler + syscall wrappers
├── state/           Atomic state with flock
├── lima/            Lima VM management (macOS only)
└── vm/              Apple VZ backend (experimental)

sdk/                 Public Go SDK
sdks/python/         Python SDK (PyPI: mvm-sandbox)
sdks/typescript/     TypeScript SDK (npm: @agentstep/mvm-sdk)

scripts/
└── install-cloud.sh Self-contained cloud bootstrap script
```

~8K LOC Go, ~1K LOC Python+TS combined. Built on Firecracker, Lima, and userfaultfd.

## Status

- Cloud mode shipped and tested on GCP
- UFFD lazy restore shipped (functional verified; final timing benchmark pending)
- Python + TypeScript + Go SDKs published and tested
- Install script bootstraps a Linux server in 95 seconds
- End-to-end verified: install, TLS, auth, VM lifecycle, exec, snapshot, pool
- Open source (Apache 2.0) at github.com/agentstep/mvm

## Roadmap

**Near-term**
1. Clean UFFD vs File timing benchmark (pending test infra fix)
2. Tiered cold storage for snapshots (Sprites-style)
3. Reflink-based snapshot create (sub-second on supporting filesystems)
4. Pool size per-workload configuration

**Mid-term**
5. Multi-tenancy (project namespaces, per-tenant API keys)
6. Structured logging and metrics
7. GPU passthrough (requires QEMU backend)
8. Get listed as a [ComputeSDK](https://www.computesdk.com) provider

**Not in scope**
- Docker packaging (intentionally — Firecracker needs bare metal)
- Hosted SaaS (intentionally — we're the self-hosted alternative)
