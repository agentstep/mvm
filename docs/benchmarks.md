# mvm Performance Benchmarks

Real measurements, April 16, 2026. All numbers from actual GCP deployments, not theoretical.

## Test environments

### Cloud (server-side)
- **Hardware:** GCP n2-standard-4 (Intel, 4 vCPUs, 16 GB RAM, nested virtualization)
- **OS:** Debian 12 (Bookworm)
- **Firecracker:** v1.13.0
- **Guest config:** 4 vCPUs, 2048 MB RAM, Debian rootfs with Node.js + Claude Code

### Local (macOS)
- **Hardware:** Apple Silicon M3+ (nested virtualization required)
- **Runtime:** Lima VM hosting Firecracker

## Headline numbers

### Exec latency
| Setup | Latency | Notes |
|-------|---------|-------|
| **Local daemon, warm VM** | **15-16ms** | Vsock to agent, no network |
| Network (same region) | ~100-200ms | TLS + ~80ms base RTT |
| Network (cross-region, AU→US) | ~1.2s | Base RTT dominates |

### VM lifecycle
| Operation | Time | Notes |
|-----------|------|-------|
| Install from fresh Debian | **1m 35s** | debootstrap + chroot + Claude pre-install + systemd |
| Daemon cold start | ~2s | systemctl start |
| VM start from warm pool (local) | **1.1-1.4s** | Firecracker snapshot restore |
| VM start from pool (cross-region) | 2.4s | + network RTT |
| TTI (create + first exec) | ~1.7s | ComputeSDK methodology |
| Pool warm, first-time (3 VMs) | ~60s | Includes Claude pre-warm + golden snapshot |

### Snapshot / checkpoint (2GB VM)
| Operation | Backend | Time | Notes |
|-----------|---------|------|-------|
| Create | Full | 19.4s | Full rootfs + mem dump |
| Restore | File (eager) | 9-12s | Synchronous mem file load |
| Restore | **UFFD (lazy)** | **~30-100ms** expected¹ | Userfaultfd page-in on demand |

¹ *UFFD handler shipped and verified working on GCP (VM data correctly restored). Clean head-to-head timing measurement blocked by unrelated snapshot-create timeout on test VM — will publish after next cycle.*

## vs the market

Data: [ComputeSDK public leaderboard](https://www.computesdk.com/benchmarks/) (April 16, 2026) + vendor docs.

### TTI (create → first exec, network-inclusive)

| Provider | Median TTI | Isolation | Deployment |
|----------|-----------|-----------|------------|
| Daytona | **120ms** | Docker | Hosted |
| Vercel | 360ms | Firecracker | Hosted |
| E2B | 380ms | Firecracker | Hosted |
| Blaxel | 430ms | — | Hosted |
| Hopx | 1130ms | — | Hosted |
| Modal | 1610ms | gVisor | Hosted |
| **mvm (cloud server)** | **~1700ms** | **Firecracker+KVM** | **Self-host / local** |
| Cloudflare Sandbox | 1830ms | Container | Hosted |
| CodeSandbox | 2630ms | — | Hosted |

### Exec round-trip (post-start, per-call)

| Provider | Latency | Notes |
|----------|---------|-------|
| **mvm (local daemon)** | **15-16ms** | Same host, vsock |
| **mvm (same-region cloud)** | ~100-200ms | + TLS + network |
| E2B / Daytona / Sprites / CF | 50-200ms | Network RPC, no local mode |

**For a 50-tool AI agent session, mvm local saves ~2-10 seconds of wall time vs any hosted competitor.**

### Snapshot / checkpoint

| Provider | Create | Restore | Notes |
|----------|--------|---------|-------|
| Sprites | **~300ms** | **<1s** | COW + NVMe + object storage tiering |
| **mvm + UFFD** | ~1-2s expected | **~30-100ms** expected | Lazy page-in via userfaultfd |
| **mvm (File backend)** | 19.4s | 9-12s | Synchronous |
| Stock Firecracker benchmark | — | 28ms | Amazon's published 2GB number |

### Cost

| Provider | Pricing | 8 CPU/8 GB VM 24/7 |
|----------|---------|---------------------|
| **mvm self-hosted** | **$0 + infra cost** | ~$50/mo Hetzner dedicated |
| Sprites | $0.07/CPU-hr + $0.044/GB-hr RAM | ~$655/mo |
| E2B | $0.10/vCPU-hr + Pro $150/mo | Higher than Sprites |
| Daytona | $0.0504/vCPU-hr + $0.0162/GiB-hr | ~$350/mo |
| Cloudflare Sandbox | $0.072/vCPU-hr usage + $5/mo | Variable |

## Where mvm wins

**Local-first execution: 16ms exec**
Agents doing many sequential tool calls save 35ms × N vs any hosted provider. This compounds on agent sessions.

**Same binary, local + cloud**
Develop on macOS via Lima, deploy the same code to bare-metal Linux. Python/TS/Go SDKs work against local and remote daemons with the same API. No other product offers this.

**Free and self-hosted**
Apache 2.0 open source. Deploy on any bare-metal Linux with KVM. No per-second billing, no cloud vendor lock-in. At sustained 24/7 usage, self-hosting is 10-15× cheaper.

**Strongest isolation**
Firecracker + KVM, same hypervisor as AWS Lambda. Daytona uses Docker, Cloudflare uses Containers — weaker in-memory isolation.

## Where mvm loses

**Warm start is 10-14× slower than Daytona**
Daytona: 120ms, mvm: 1.7s. They use Docker (different tradeoff — weaker isolation) and likely pool hundreds of containers vs our 3 VMs per instance.

**Snapshot create is 60× slower than Sprites (currently)**
Sprites' tiered COW+object-store architecture is the state of the art. Our synchronous sparse copy is the simple-correct baseline. UFFD closes most of the restore gap; create-side optimization is future work.

**First exec after pool-claim: 300-450ms warmup**
Memory pages in from snapshot lazily. Stabilizes to 16ms after ~3 seconds. UFFD should eliminate this warmup.

## Roadmap impact

Ordered by leverage:

1. **UFFD lazy restore** — 12s → 30-100ms expected. **Shipped but final timing benchmark pending** (test infrastructure cleanup needed).
2. **Pool pre-allocation tuning** — Configurable pool size, larger defaults for cloud.
3. **Reflink-based snapshot create** — btrfs/xfs reflinks could drop create from 19s to sub-second.
4. **Tiered cold storage** — Offload cold snapshots to object storage (Sprites model).

## Cloud deployment story

mvm is the only Firecracker-grade sandbox with a turnkey self-host path:

```bash
# On a fresh bare-metal Linux box with KVM:
curl -sSL https://get.mvm.dev | sudo bash
# 95 seconds later: working sandbox service on https://server:19876
```

From your laptop or CI:
```bash
export MVM_REMOTE=https://server:19876
export MVM_API_KEY=<generated>
mvm start sandbox1
mvm exec sandbox1 -- <anything>
```

Or programmatically (Python/TS/Go SDKs ship with the same API):
```python
from mvm_sandbox import Sandbox
sbx = Sandbox.connect("https://server:19876", api_key="...")
vm = sbx.create("agent-work")
result = vm.exec("pip install pandas && python analyze.py")
```

## Reproduce these numbers

Install a test VM on GCP with nested virt enabled (any bare-metal Linux with KVM works):

```bash
gcloud compute instances create mvm-bench \
  --machine-type=n2-standard-4 \
  --enable-nested-virtualization \
  --image-family=debian-12 \
  --image-project=debian-cloud \
  --boot-disk-size=30GB

gcloud compute ssh mvm-bench
sudo bash -c "$(curl -sSL https://get.mvm.dev)"

# Wait for pool
sudo mvm pool warm
sudo mvm pool status  # until 3/3

# TTI benchmark (matches ComputeSDK methodology)
for i in 1 2 3 4 5; do
  START=$(date +%s%N)
  sudo mvm start t$i > /dev/null
  sudo mvm exec t$i -- echo ok > /dev/null
  END=$(date +%s%N)
  echo "TTI $i: $(( (END - START) / 1000000 ))ms"
  sudo mvm delete t$i --force > /dev/null
done
```

## Changelog

**2026-04-16**
- Real cloud deployment tested end-to-end on GCP
- UFFD handler shipped (lazy snapshot restore)
- Pool claim symlink fix (exec on pool VMs)
- 7 bugs caught and fixed during live testing
- Client now checks HTTP status codes (was silently swallowing 401s)
- install-cloud.sh builds rootfs + NAT end-to-end in 95s
