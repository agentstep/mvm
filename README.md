# mvm — Firecracker MicroVMs, local or self-hosted

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Hardware-isolated Linux sandboxes for AI agents. **Same binary runs on your Mac and on your servers.** No cloud vendor. Same API.

```bash
# On your Mac:
mvm start sandbox                      # 1.4s, claimed from warm pool
mvm exec sandbox -- npm test           # 16ms exec latency — vsock to agent

# Or on a Linux server you own:
curl -sSL https://get.mvm.dev | sudo bash    # 95s: fresh box → working sandbox service
export MVM_REMOTE=https://my-server:19876
mvm exec sandbox -- npm test           # same CLI, same API
```

## Why

AI coding agents need root, shell, and network access to do real work. The options available today:

- **Docker** — namespace isolation, shared kernel, one CVE from container escape
- **Cloud sandboxes (E2B, Daytona, Sprites, Cloudflare)** — real isolation but your code and credentials leave your machine, with per-second billing
- **mvm** — Firecracker+KVM hardware isolation, local-first, optional self-hosted, free

mvm is the only product that gives you the same sandbox API locally and on infrastructure you control. Dev on your Mac, deploy to your own servers, no code changes.

## Quick start

### On macOS (local mode)

```bash
# Requires Apple Silicon M3+ (nested virtualization)
brew install agentstep/tap/mvm
mvm init

mvm start sandbox
mvm exec sandbox -- echo hello
```

### On bare-metal Linux with KVM (cloud mode)

```bash
# Any Linux host with /dev/kvm — AWS .metal, GCP nested-virt, Hetzner dedicated, etc.
curl -sSL https://get.mvm.dev | sudo bash

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

**For AI agents making many tool calls, mvm's 16ms local exec wins decisively** — a 50-call session saves 2-10 seconds vs any hosted provider.

## Network sandboxing

Per-VM network policies — most prompt injection attacks need exfiltration, so blocking network kills them:

```bash
mvm start sandbox --net-policy deny                        # no outbound
mvm start sandbox --net-policy allow:github.com,npmjs.org  # allowlist
```

## Pause, resume, and snapshot

Firecracker supports full memory-state checkpoints. Use them as a habit:

```bash
mvm pause sandbox            # freeze VM in memory, zero CPU
mvm resume sandbox           # instant resume
mvm snapshot create sandbox before-install
mvm exec sandbox -- risky-install.sh
mvm snapshot restore sandbox before-install     # roll back full VM state
```

No Virtualization.framework-based tool on macOS can do memory-state pause/resume. Requires Firecracker's snapshot support, which is why mvm uses nested KVM.

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
- `mvm exec <name> -- <cmd>` — run a command (`-it`, `-e`, `-w`)
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

| Product | Isolation | Local dev | Self-host | Cost model |
|---------|-----------|-----------|-----------|------------|
| **mvm** | Firecracker+KVM | **Yes (macOS/Linux)** | **Yes** | **Free, open source** |
| E2B | Firecracker+KVM | No | No | $0.10/vCPU-hr + Pro $150/mo |
| Daytona | Docker | No | Enterprise | $0.05/vCPU-hr |
| Sprites (Fly.io) | Firecracker+KVM | No | No | $0.07/CPU-hr + storage |
| Cloudflare Sandbox | Container | No | No | $0.072/vCPU-hr + $5/mo |
| microsandbox | libkrun | Yes (local) | No server | Free (local only) |
| Docker Sandboxes | microVM | Yes | Yes | Proprietary |

**mvm is the only Firecracker-grade product that works both locally and as a self-hosted service.**

## Requirements

### Local (macOS)
- Apple Silicon M3 or newer (nested virtualization requires M3+)
- macOS 15 (Sequoia) or newer
- Homebrew

### Cloud (Linux)
- Bare-metal Linux with `/dev/kvm` accessible
- Supported providers: AWS `.metal` instances, GCP with nested virt enabled, Hetzner dedicated, OVH bare metal, any physical Linux server
- **Not supported:** Standard cloud VMs (EC2 t3, GCP e2, etc.) unless nested virt is explicitly enabled

## Architecture

### Local mode
```
mvm (macOS) → Unix socket → daemon (in Lima VM) → Firecracker microVMs
```

[Lima](https://github.com/lima-vm/lima) provides Linux with nested virtualization. [Firecracker](https://github.com/firecracker-microvm/firecracker) runs inside Lima.

### Cloud mode
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
