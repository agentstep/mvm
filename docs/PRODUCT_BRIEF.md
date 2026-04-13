# mvm — Product Brief

## One-liner

Hardware-isolated Linux VMs on your Mac in 0.35 seconds. Safe sandboxes for AI coding agents.

## Problem

AI coding agents (Claude Code, Gemini, Codex, OpenCode) need root access, shell access, and network access to do useful work. Running them on your host machine is dangerous — one prompt injection or hallucinated `rm -rf` away from losing your data.

Docker gives agents a sandbox, but it's namespace isolation — a shared kernel, [one CVE away from escape](https://nvd.nist.gov/vuln/detail/CVE-2024-21626). Cloud sandboxes (E2B, Sprites) give real isolation, but your code and credentials leave your machine.

Developers need hardware-isolated VMs that are fast enough to feel like containers, local enough to keep secrets on-device, and simple enough to use without reading docs.

## Solution

mvm gives each AI agent its own Firecracker microVM on your Mac. Separate kernel per VM. KVM hardware isolation — the same technology behind AWS Lambda. Your secrets stay local. Network locked down per-VM.

```bash
mvm start sandbox --net-policy deny
mvm ssh sandbox
claude --dangerously-skip-permissions    # full root access, can't touch your Mac
```

## Key metrics

| Metric | mvm | Apple Containers | Docker |
|--------|-----|-----------------|--------|
| Start VM | **0.35s** | 0.95s | ~3s |
| Run command | **0.28s** | 0.95s | ~1.5s |
| Pause (checkpoint) | **0.15s** | — | — |
| Resume | **0.14s** | — | — |
| Memory per VM | ~25MB | ~30MB | ~150MB |
| Isolation | Hardware (KVM) | Hardware (VZ) | Namespace |

## How it works

```
macOS → Lima VM (Virtualization.framework) → Firecracker microVMs (KVM)
```

[Lima](https://github.com/lima-vm/lima) provides a Linux environment with nested virtualization on Apple Silicon. [Firecracker](https://github.com/firecracker-microvm/firecracker) runs inside Lima, creating microVMs with hardware-level isolation via KVM. A warm pool pre-boots VMs so `mvm start` claims one instantly.

On M1/M2 Macs (no nested virt support), an experimental Apple Virtualization.framework backend runs VMs directly — no Lima, no nesting.

## Features

**Core VM lifecycle**
- `start`, `stop`, `pause`, `resume`, `ssh`, `exec`, `logs`, `list`, `delete`
- Warm pool for 0.35s instant starts
- Live boot log streaming during startup
- Pause/resume with full memory-state checkpoint (0.15s/0.14s)

**Security**
- Per-VM network sandboxing: `--net-policy deny` or `--net-policy allow:github.com`
- Seccomp profiles: `--seccomp strict`
- Hardware isolation via KVM (separate kernel per VM)
- Filesystem diff tracking: `mvm diff` shows what an agent changed

**Developer experience**
- `mvm install` — run package installs at native speed (10x faster than inside VM)
- `mvm doctor` — system diagnostics
- `mvm update` — self-update from GitHub releases
- `--watch` mode for file sync on changes
- `-p 8080:80` port forwarding
- `-V ./src:/app` volume mounts
- Template presets: `mvm template init --preset node`
- `/setup-mvm` Claude Code skill for AI-guided onboarding

**Pre-installed AI agents**
- Claude Code (`claude`) — Anthropic
- Gemini CLI (`gemini`) — Google
- Codex CLI (`codex`) — OpenAI
- OpenCode (`opencode`) — open source


Each VM includes a skills file (`/.mvm/SKILLS.md`) that teaches agents the VM environment.

**Snapshots**
- Delta snapshots (capture only changes since base)
- AES-256-GCM encrypted snapshots (`MVM_SNAPSHOT_KEY` env var)
- `mvm snapshot create/restore/list/delete`

**Auto-idle**
- `mvm idle enable --timeout 5m` — installs macOS LaunchAgent
- VMs auto-pause after inactivity, auto-resume on next `exec`/`ssh`
- Zero CPU while paused

**Two backends**
- Firecracker (M3+): full features including pause/resume, snapshots, warm pool
- Apple VZ (M1+, experimental): no pause/resume, no snapshots

## Target user

A developer on a Mac who wants to run AI coding agents safely. Not a platform team managing a fleet. Not a cloud service.

The user runs `mvm init`, starts a VM, SSHes in, and runs Claude Code with `--dangerously-skip-permissions` knowing the agent has full root access inside an isolated VM that can't touch their host filesystem or network.

## Competitive landscape

| Tool | Isolation | Start | Pause | Net sandbox | Min chip |
|------|-----------|-------|-------|-------------|----------|
| **mvm** | KVM | **0.35s** | **Yes** | **Yes** | M3+ |
| Shuru | VZ | ~1s | Disk only | Yes | M1+ |
| microsandbox | KVM (libkrun) | <0.2s | No | Yes | M1+ |
| Apple Containers | VZ | ~0.95s | No | No | M1+ |
| Docker Sandboxes | microVM | ~3s | No | Yes | M1+ |

mvm is the only local tool with full memory-state pause/resume. The M3+ requirement for Firecracker is the main tradeoff — the Apple VZ backend addresses this for M1/M2 users.

## Architecture

```
mvm/
├── cmd/mvm/              Go CLI (Cobra)
├── internal/
│   ├── cli/              22 commands
│   ├── lima/             Lima VM management
│   ├── firecracker/      FC lifecycle, pool, snapshots, chroot, security
│   ├── state/            Atomic state with flock
│   └── vm/               Apple VZ backend
├── agent/                vsock guest agent (built, not yet wired)
├── vz/                   Swift helper for Apple VZ (1.6MB)
└── scripts/              Integration + OpenCode tests
```

~6K LOC Go, 75 LOC Swift. One dependency (spf13/cobra). 67 unit tests + 33 integration tests.

## Status

- v0.1.0 released, 45+ commits
- Open source (Apache 2.0 license) at github.com/agentstep/mvm
- CI green (GitHub Actions)
- End-to-end tested: full lifecycle, OpenCode post-install, pause/resume, network sandboxing
- Three rounds of code review (architect + Codex GPT-5.4): 21 bugs found and fixed

## What's next

1. Wire vsock guest agent into SSH call sites (built, needs integration)
2. VirtioFS for Apple VZ volume mounts
3. `mvm pull` — downloadable pre-built images (skip npm installs entirely)
4. Per-VM `--cpus` and `--memory` flags
5. Community: HN launch, gather feedback, iterate
