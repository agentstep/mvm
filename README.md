# mvm — Firecracker MicroVMs on macOS

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![macOS](https://img.shields.io/badge/macOS-15%2B-000000?logo=apple&logoColor=white)](https://www.apple.com/macos/)
[![Apple Silicon](https://img.shields.io/badge/Apple%20Silicon-M3%2B-333333?logo=apple&logoColor=white)](https://support.apple.com/en-us/116943)

Disposable computers on your Mac. Hardware-isolated Linux VMs in 0.35s.

```bash
$ mvm start sandbox --net-policy deny   # 0.35s, network locked down
$ mvm ssh sandbox
$ claude --dangerously-skip-permissions  # full root access, can't touch your Mac
```


## Why

AI coding agents need root access, shell access, and network access. Docker gives them that behind a shared kernel — [one CVE away from a container escape](https://nvd.nist.gov/vuln/detail/CVE-2024-21626). Cloud sandboxes give them real isolation, but your code and credentials leave your machine.

mvm gives each agent its own hardware-isolated VM on your Mac. Separate kernel per VM via KVM. Your secrets stay local. Network locked down per-VM. Fast enough to feel instant.

## Setup

### Requirements

- Apple Silicon **M3 or newer** (nested virtualization requires M3+)
- macOS **15 (Sequoia)** or newer
- [Homebrew](https://brew.sh)
- [Claude Code](https://claude.ai/code) (recommended for guided setup)

### Option A: AI-guided setup (recommended)

```bash
git clone https://github.com/agentstep/mvm.git
cd mvm
claude
> /setup-mvm
```

Claude Code handles everything: checking your hardware, installing dependencies, configuring Lima and Firecracker, building the base image, warming the pool, and walking you through your first VM. It troubleshoots errors as they come up. No docs required.

### Option B: npm

```bash
npm install -g @agentstep/mvm
mvm init
```

### Option C: Homebrew

```bash
brew install agentstep/tap/mvm
mvm init
```

### Option D: From source

```bash
git clone https://github.com/agentstep/mvm.git
cd mvm
make install
mvm init                      # or: mvm init --minimal (no AI agents, smaller image)
```

## Quick Start

```bash
# Instant starts with the warm pool
mvm pool warm                 # pre-boot in background (~10s)
mvm start my-app              # 0.35s — claimed from pool

# Or boot fresh (streams boot log until SSH ready)
mvm start my-app

# SSH in
mvm ssh my-app
```

## Pause & Resume

Checkpoint full VM state — memory, CPU registers, everything — in 0.15s. Resume in 0.14s. The guest doesn't know it was ever frozen.

Use this as a habit, not an escape hatch. Pause before risky operations the way you'd hit Cmd-S. Agent about to install a pile of npm packages from a hallucinated package.json? Pause first. Agent wants to rewrite your config? Pause first. If it goes sideways, resume.

```bash
mvm pause my-app    # 0.15s — freeze VM, zero CPU, state preserved in memory
mvm resume my-app   # 0.14s — instant resume, full state
```

No Virtualization.framework-based tool on macOS can do this. It requires Firecracker's snapshot support, which is why mvm uses nested virtualization via KVM.

## Network Sandboxing

Most prompt injection attacks need exfiltration — the injected instructions tell the agent to curl something or post data somewhere. If the VM can't reach the internet, most of those attacks are dead on arrival.

```bash
mvm start sandbox --net-policy deny                        # block all outbound
mvm start sandbox --net-policy allow:api.anthropic.com,github.com  # allowlist only
```

## Usage

### Run commands

```bash
mvm exec my-app -- apt update
mvm exec my-app -it -- bash         # interactive shell
mvm exec my-app -e FOO=bar -- env   # environment variables
mvm exec my-app -w /tmp -- pwd      # working directory
```

### Port forwarding

```bash
mvm start web -p 8080:80                  # host:8080 -> guest:80
mvm start api -p 3000:3000 -p 5432:5432   # multiple ports
```

### Logs

```bash
mvm logs my-app                # guest system log
mvm logs my-app -f             # follow (stream)
mvm logs my-app --boot -f      # stream boot log live
```

### Lifecycle

```bash
mvm list                       # show all VMs
mvm stop my-app                # graceful shutdown
mvm stop my-app --force        # kill immediately
mvm delete my-app              # remove all resources
mvm delete --all --force       # nuke everything
```

## Performance

| Operation | mvm (warm pool) | Apple Containers | Docker |
|-----------|----------------|-----------------|--------|
| **Start VM** | **0.35s** | 0.95s | ~3s |
| **Run command** | **0.28s** | 0.95s | ~1.5s |
| **Pause** | **0.15s** | — | — |
| **Resume** | **0.14s** | — | — |
| **Memory per VM** | ~25MB | ~30MB | ~150MB |
| **Isolation** | Hardware (KVM) | Hardware (VZ) | Namespace |

10 concurrent VMs: ~450MB total. Docker Desktop: ~3GB for the same.

## Pre-installed Tools

Each VM comes with:
- **Claude Code** (`claude`) — Anthropic
- **Gemini CLI** (`gemini`) — Google
- **Codex CLI** (`codex`) — OpenAI
- **OpenCode** (`opencode`) — open source
- **Node.js** 22.x, **Python** 3.12, **git**, **curl**, **wget**

Use `mvm init --minimal` for a slim image without AI agents.

## Self-Documenting VMs

Each VM includes a file at `/.mvm/SKILLS.md` that teaches agents the VM environment — capabilities, port management, checkpoints, and networking. Agents don't need you to explain the sandbox to them.

## Commands

| Command | Description |
|---------|-------------|
| `mvm init` | One-time setup (`--minimal` for slim image without agents) |
| `mvm start <n>` | Create and boot a microVM (`-p`, `--net-policy`, `-d`) |
| `mvm stop <n>` | Graceful shutdown (`--force` to kill) |
| `mvm pause <n>` | Checkpoint: freeze VM in memory (zero CPU) |
| `mvm resume <n>` | Resume a paused VM instantly |
| `mvm ssh <n>` | SSH into a running microVM |
| `mvm exec <n> -- <cmd>` | Run a command (`-it`, `-e`, `-w`, `-u`) |
| `mvm logs <n>` | View logs (`--boot`, `-f`, `-n`) |
| `mvm list` | List all microVMs (`--json`, `-q`) |
| `mvm delete <n>` | Delete a microVM (`--force`, `--all`) |
| `mvm pool warm` | Pre-boot VMs for instant starts |
| `mvm pool status` | Show warm pool status |

## Comparison

| Tool | Isolation | Start | Pause/Resume | Net sandbox | Min chip | License |
|------|-----------|-------|-------------|-------------|----------|---------|
| **mvm** | KVM (Firecracker) | **0.35s** | **Yes (full memory state)** | **Yes** | M3+ | Apache-2.0 |
| [Shuru](https://github.com/superhq-ai/shuru) | VZ (Apple) | ~1s | Disk only | Yes | M1+ | Apache-2.0 |
| [microsandbox](https://github.com/zerocore-ai/microsandbox) | KVM (libkrun) | <0.2s | No | Yes | M1+ | Apache-2.0 |
| [Apple Containers](https://github.com/apple/container) | VZ (Apple) | ~0.95s | No | No | M1+ | Apache-2.0 |
| [Docker Sandboxes](https://docs.docker.com/ai/sandboxes/) | microVM | ~3s | No | Yes | M1+ | Proprietary |

### Two backends

| | Firecracker (default) | Apple VZ (experimental) |
|---|---|---|
| **Chips** | M3+ | M1+ |
| **Pause/resume** | Yes | No |
| **Warm pool** | Yes (0.35s) | No |
| **Init** | `mvm init` | `mvm init --backend applevz` |

Auto-detection: M3+ → Firecracker, M1/M2 → Apple VZ. Override with `--backend`.

The Apple VZ backend is **experimental** — no pause/resume, no warm pool, no snapshots. For mature M1/M2 alternatives, see [Shuru](https://github.com/superhq-ai/shuru) or [microsandbox](https://github.com/zerocore-ai/microsandbox).

## Troubleshooting

**How do I check if my Mac supports nested virtualization?**

```bash
sysctl kern.hv_support
# kern.hv_support: 1 means hypervisor is supported
# M3+ is required for nested virtualization within a VM
```

Check your chip: Apple menu → About This Mac. If it says M1 or M2, mvm won't work — see [alternatives](#the-m3-tradeoff) above.

**`mvm init` fails or hangs**

This usually means Lima isn't configured correctly. Run `mvm init` again with verbose output:

```bash
mvm init --verbose
```

If the issue persists, [open an issue](https://github.com/agentstep/mvm/issues) with the output.

**VMs won't start from warm pool**

Make sure the pool is running:

```bash
mvm pool status
mvm pool warm    # restart the pool if needed
```

**Network policy isn't blocking traffic**

Verify the policy is applied:

```bash
mvm list         # check the net-policy column
```

If you set the policy after start, stop and restart the VM with the `--net-policy` flag — policies are set at boot.

## How It Works

[Lima](https://github.com/lima-vm/lima) provides a Linux VM with nested virtualization on Apple Silicon. [Firecracker](https://github.com/firecracker-microvm/firecracker) runs inside Lima, creating microVMs with hardware-level isolation via KVM — the same isolation model behind AWS Lambda. The **warm pool** pre-boots VMs in the background so `mvm start` claims one instantly.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

If you find a bug or have a feature request, [open an issue](https://github.com/agentstep/mvm/issues). If you'd like to contribute code, fork the repo, create a branch, and open a pull request.

## Changelog

See [Releases](https://github.com/agentstep/mvm/releases) for version history and release notes.

## Acknowledgments

Built on [Firecracker](https://github.com/firecracker-microvm/firecracker) and [Lima](https://github.com/lima-vm/lima). Inspired by [Fly.io Sprites](https://sprites.dev/) and [yashdiq/firecracker-lima-vm](https://github.com/yashdiq/firecracker-lima-vm).

## License

[Apache 2.0](LICENSE)
