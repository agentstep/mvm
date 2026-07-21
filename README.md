# mvm — hardware-isolated microVMs, local or self-hosted

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Hardware-isolated Linux sandboxes for AI agents. **Native microVMs on Apple Silicon, Firecracker on the Linux servers you own — same binary, same API.** No cloud vendor.

```bash
# On your Mac — native Apple Silicon microVM, no nested layer:
mvm start sandbox                       # ~0.7s cold boot to an agent-ready VM
mvm exec sandbox npm test            # exec over vsock to the in-guest agent
mvm pause sandbox && mvm resume sandbox # freeze in memory at zero CPU, then resume
# (memory+disk save/restore also exists — see Snapshots; note the macOS 26.2 caveat)

# Or on a Linux server you own:
curl -fsSL https://raw.githubusercontent.com/agentstep/mvm/main/scripts/install-cloud.sh | sudo bash
export MVM_REMOTE=https://my-server:19876
mvm exec sandbox npm test           # same CLI, same API
```

## Why

AI coding agents need root, shell, and network access to do real work. The options available today:

- **Docker** — namespace isolation, shared kernel, one CVE from container escape
- **gVisor (Modal)** — userspace syscall interception, not a hardware-virtualized VM
- **Cloud sandboxes (E2B, Sprites, Cloudflare, Vercel)** — real isolation but your code and credentials leave your machine, with per-second billing
- **mvm** — hardware-isolated microVMs (Apple Virtualization.framework on macOS, Firecracker+KVM on Linux), local-first, optional self-hosted, free

mvm is the only hardware-isolated sandbox that runs natively on your Mac **and** self-hosts on infrastructure you control, behind one API. Dev on your Mac, deploy to your own servers, no code changes.

## Quick start

### On macOS (local mode)

mvm has two macOS backends. Pick at `init`:

```bash
# Build from source (Go + Swift toolchains required). Installs mvm and the
# codesigned mvm-vz helper into $(go env GOPATH)/bin — make sure that's on PATH:
#   export PATH="$(go env GOPATH)/bin:$PATH"
git clone https://github.com/agentstep/mvm.git && cd mvm
make install

# Native Apple Virtualization.framework — fastest, runs on any Apple Silicon (M1+),
# no nested layer. ~0.7s cold boot, ~0.3s save/restore.
mvm init --backend applevz

# — or — Firecracker nested in a Lima VM (needs M3+). Adds the full Firecracker
# feature set: warm pool, UFFD lazy restore, named multi-snapshot history.
mvm init --backend firecracker

mvm start sandbox
mvm exec sandbox echo hello
```

### On bare-metal Linux with KVM (cloud mode)

```bash
# Any Linux host with /dev/kvm — AWS .metal, GCP nested-virt, Hetzner dedicated, etc.
curl -fsSL https://raw.githubusercontent.com/agentstep/mvm/main/scripts/install-cloud.sh | sudo bash

# The install script:
#  - Downloads mvm + Firecracker
#  - Builds a Debian rootfs
#  - Generates TLS cert + API key
#  - Installs systemd unit
#  - Total: ~95 seconds

# Connect from anywhere
export MVM_REMOTE=https://server:19876
export MVM_API_KEY=$(ssh server cat /etc/mvm/api-key)
mvm pool status
mvm start sandbox
```

### From Python / TypeScript / Go

```python
# pip install mvm-sandbox
from mvm_sandbox import Sandbox
client = Sandbox.connect("https://server:19876", api_key="...")
vm = client.create("agent-work", cpus=2, memory_mb=512)
result = vm.exec("pip install pandas && python analyze.py")
vm.snapshot("before-risky-op")
vm.exec("risky operation")
vm.restore("before-risky-op")  # roll back if needed
vm.delete()
```

```typescript
// npm install @agentstep/mvm-sdk
import { Sandbox } from '@agentstep/mvm-sdk';
const client = new Sandbox({ remote: 'https://server:19876', apiKey: '...' });
const vm = await client.create('agent-work');
const r = await vm.exec('npm install && npm test');
```

```go
// go get github.com/agentstep/mvm/sdk
import "github.com/agentstep/mvm/sdk"
client := sdk.New("https://server:19876", sdk.WithAPIKey("..."))
vm, _ := client.CreateVM(ctx, sdk.CreateVMRequest{Name: "agent-work"})
result, _ := client.Exec(ctx, "agent-work", "uname -a")
```

## Performance

### macOS — native Apple VZ backend

Measured on Apple Silicon, June 2026. Every sample verified *running + agent-responding* before counting (n=6 cold boot, n=8 restore, zero failures):

| Operation | mvm (Apple VZ) | Notes |
|-----------|----------------|-------|
| **Cold boot** (fresh VM → agent ready) | **0.697s** (0.660–0.718s) | native, no nested layer |
| **Fast-restore** (memory + disk checkpoint) | **0.293s** (0.289–0.353s)¹ | vs Fly Sprites' published ~300ms — and local, not cloud |
| **Disk rollback** on restore | ✅ works¹ | files written after the checkpoint vanish |

These are single-machine numbers on Apple Silicon; cross-machine and in-guest I/O (fio) figures aren't published yet.

> ¹ **Restore is currently broken on macOS 26.2.** The 0.293s restore + disk
> rollback were verified on macOS ≤26.1, but macOS 26.2 hardened Virtualization.
> framework to reject the ad-hoc-signed helper on the restore path
> (`VZError 12 "permission denied"`). Fix in progress: sign the helper with a
> Developer ID + provisioning profile carrying the virtualization entitlement.
> Cold boot, exec, and pause/resume are unaffected.

### Linux — Firecracker (cloud mode)

Real measurements on GCP n2-standard-4, April 2026. See [`docs/benchmarks.md`](docs/benchmarks.md) for full comparison.

| Operation | mvm | Competitors |
|-----------|-----|-------------|
| **Exec (local daemon, warm)** | **16ms** | E2B/Daytona/Sprites: 50-200ms (network) |
| **TTI (create + first exec)** | 1.7s | Daytona 120ms, E2B 380ms, CF 1830ms |
| **VM start from pool** | 1.1-1.4s | — |
| **Install from scratch** | 95s | N/A (hosted only) |
| **Snapshot create (2GB)** | 19.4s | Sprites ~300ms |
| **Snapshot restore (UFFD)** | ~30-100ms target¹ | Stock Firecracker: 28ms |
| **Cost (8 CPU/8 GB, 24/7)** | **~$50/mo self-host** | Sprites $655/mo, E2B higher |

¹ UFFD lazy restore shipped and verified functional; clean timing benchmark pending.

For agents making many tool calls, the 16ms local exec adds up: a 50-call session saves roughly 2–10s of exec overhead versus a network round-trip to a hosted provider. (Local-daemon number; your latency to a remote daemon is network-bound like anyone else's.)

## Network sandboxing

Per-VM network policies — most prompt injection attacks need exfiltration, so blocking network kills them:

```bash
mvm start sandbox --net-policy deny                        # no outbound
mvm start sandbox --net-policy allow:github.com,npmjs.org  # allowlist
```

## Pause, resume, and snapshot

Both backends do full memory-state checkpoints — freeze the whole machine, restore it exactly, roll the disk back. Use them as a habit:

```bash
mvm pause sandbox            # freeze VM in memory, zero CPU
mvm resume sandbox           # instant resume
mvm snapshot create sandbox  # checkpoint memory + disk
mvm exec sandbox risky-install.sh
mvm stop sandbox && mvm start sandbox   # restore: memory and disk roll back
```

- **Apple VZ (macOS):** uses Virtualization.framework's `saveMachineStateTo` / `restoreMachineStateFrom` (macOS 14+) for ~0.29s fast-restore, plus an APFS copy-on-write disk clone for instant rollback. A running process survives the round-trip; any file written after the checkpoint is gone on restore.
- **Firecracker (Linux):** full memory snapshots with UFFD lazy (page-in-on-demand) restore and named, multi-snapshot history (`mvm snapshot create/restore/list`).

## Custom images

Extend the base with a Dockerfile. mvm parses it and builds a rootfs (no Docker required):

```dockerfile
# my-agent.Dockerfile
FROM mvm-base
RUN apt-get update && apt-get install -y postgresql-client redis-tools
ENV DATABASE_URL=postgres://localhost/dev
```

```bash
mvm build -f my-agent.Dockerfile -t my-agent
mvm start sandbox --image my-agent
```

## Commands

### VM lifecycle
- `mvm start <name>` — create from warm pool (`-p`, `--net-policy`, `--image`, `--cpus`, `--memory`)
- `mvm exec <name> <cmd>` — run a command (`-it`, `-e`, `-w`)
- `mvm stop <name>` — graceful shutdown (`--force`)
- `mvm pause <name>` / `mvm resume <name>` — memory-state checkpoint
- `mvm list` — show all VMs (`--json`)
- `mvm delete <name>` — clean up (`--force`, `--all`)

### Pool
- `mvm pool warm` — pre-boot VMs for instant starts
- `mvm pool status` — show pool state

### Snapshots
- `mvm snapshot create <vm> <name>` — full VM state + rootfs
- `mvm snapshot restore <vm> <name>` — restore to new VM
- `mvm snapshot list` / `mvm snapshot delete <name>`

### Custom images
- `mvm build -f Dockerfile -t <name> [--size MB]` — build custom rootfs
- `mvm images list` / `mvm images delete <name>`

### Server
- `mvm serve start` — run the daemon locally (Unix socket)
- `mvm serve start --listen 0.0.0.0:19876 --tls-cert ... --api-key-file ...` — cloud mode
- `mvm serve status` / `mvm serve stop`

### Remote mode
All commands accept `--remote https://server:19876 --api-key <key>` or via env: `MVM_REMOTE`, `MVM_API_KEY`, `MVM_CA_CERT`.

## SDKs

- **Python** — `pip install mvm-sandbox` ([PyPI](https://pypi.org/project/mvm-sandbox/))
- **TypeScript** — `npm install @agentstep/mvm-sdk`
- **Go** — `go get github.com/agentstep/mvm/sdk`

All three are thin HTTP clients against the same REST API. Work against local or remote daemons transparently.

## Competitive landscape

Competitor facts gathered June 2026 from vendors' own sites/docs — verify before relying on them.

| Product | Isolation | Native macOS | Self-host | Open source | Cost model |
|---------|-----------|--------------|-----------|-------------|------------|
| **mvm** | **Apple VZ + Firecracker/KVM** | **Yes** | **Yes** | **Apache 2.0** | **Free (your hardware)** |
| microsandbox | libkrun (KVM/HVF) | Yes | Yes | Apache 2.0 | Free / cloud beta |
| E2B | Firecracker | No | Yes (BYOC) | Apache 2.0 | ~$0.05/vCPU-hr + $150/mo Pro |
| Fly Sprites | Firecracker | No | No | Closed | $0.07/CPU-hr + storage |
| Daytona | Docker¹ (Kata opt-in) | Yes (Docker) | Yes | AGPL-3.0 | ~$0.05/vCPU-hr |
| Modal | gVisor² | No | No | Closed | ~$0.047/core-hr |
| Cloudflare | Containers + V8 isolates | No | No | SDK only | $0.00002/vCPU-sec |

¹ Daytona defaults to shared-kernel Docker containers; microVM isolation requires opting into Kata. ² Modal uses gVisor — userspace syscall interception, not a hardware-virtualized VM.

**The wedge:** every Firecracker-grade competitor is cloud-first (E2B, Sprites, Vercel) or closed (Sprites, Modal, Cloudflare); the one that's genuinely local-and-open (Daytona) defaults to plain Docker, not a microVM. **microsandbox is the only true overlap** — and against it, mvm's edge is first-party engines per OS (Apple's own hypervisor on the Mac, battle-tested Firecracker on Linux) instead of one library VMM, shipped as a single binary. mvm is the only hardware-isolated sandbox that runs natively on your Mac **and** self-hosts behind one API.

## Requirements

### Local (macOS)
- **Native Apple VZ backend:** any Apple Silicon (M1+), macOS 14+ (save/restore needs macOS 14)
- **Firecracker (Lima) backend:** Apple Silicon M3+ (nested virtualization requires M3+), macOS 15+
- Homebrew

### Cloud (Linux)
- Bare-metal Linux with `/dev/kvm` accessible
- Supported providers: AWS `.metal` instances, GCP with nested virt enabled, Hetzner dedicated, OVH bare metal, any physical Linux server
- **Not supported:** Standard cloud VMs (EC2 t3, GCP e2, etc.) unless nested virt is explicitly enabled

## Architecture

### macOS, native (Apple VZ backend)
```
mvm (macOS) → mvm-vz helper → Apple Virtualization.framework → microVM
                    │
               vsock control plane → in-guest agent (exec, PTY, net setup)
```

No nested layer — the microVM runs directly on Apple's hypervisor. The `mvm-vz` helper is a small codesigned Swift binary (needs the `com.apple.security.virtualization` entitlement); the in-guest agent speaks a vsock protocol for exec, interactive PTY, and network setup.

### macOS, Firecracker (Lima backend)
```
mvm (macOS) → Unix socket → daemon (in Lima VM) → Firecracker microVMs
```

[Lima](https://github.com/lima-vm/lima) provides Linux with nested virtualization; [Firecracker](https://github.com/firecracker-microvm/firecracker) runs inside it. Adds the full Firecracker feature set (pool, UFFD restore, named snapshots) at the cost of a nested layer and M3+.

### Cloud mode (Linux)
```
mvm / SDK → TCP+TLS → daemon (on bare-metal Linux) → Firecracker microVMs
```

Same daemon binary, same REST API. API key auth on TCP; Unix socket stays unauthenticated for local use.

## How mvm compares on agent workloads

For a typical AI coding agent doing 50 tool calls in a session:

- **mvm local** — 50 × 16ms = 800ms of exec overhead, $0
- **E2B** — 50 × 100ms = 5s of exec overhead, ~$0.01-0.05
- **Daytona** — 50 × 100ms = 5s of exec overhead, ~$0.005

Multiply by 1000 agents per day: mvm saves 80+ minutes and $5-50 per day vs hosted, before factoring in data-locality and compliance benefits.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports and feature requests: [open an issue](https://github.com/agentstep/mvm/issues).

## Acknowledgments

Built on [Firecracker](https://github.com/firecracker-microvm/firecracker), [Lima](https://github.com/lima-vm/lima), and userfaultfd(2). Inspired by [Fly.io Sprites](https://sprites.dev/) and [AWS Lambda's snapshot architecture](https://aws.amazon.com/blogs/compute/accelerating-serverless-workloads-with-aws-lambda-snapstart/).

## License

[Apache 2.0](LICENSE)
