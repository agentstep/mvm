# mvm Performance Benchmarks

Real measurements, April 16, 2026. Run these yourself with `scripts/benchmark.sh`.

## Test environment

- **Hardware:** GCP n2-standard-4 (Intel, 4 vCPUs, 16 GB RAM, nested virtualization enabled)
- **OS:** Debian 12 (Bookworm)
- **Firecracker:** v1.13.0
- **Pool size:** 3 pre-warmed VMs
- **Guest config:** 4 vCPUs, 2048 MB RAM, Debian rootfs with Node.js + Claude Code pre-installed

All measurements on server-local CLI (no network RTT). The ComputeSDK benchmark methodology (TTI = create + first exec round-trip) is used where applicable.

## Results

### Latency

| Metric | mvm | Notes |
|--------|-----|-------|
| **Exec round-trip (warm VM)** | **15-16ms** | Via Unix socket to daemon, vsock to agent |
| **Exec via curl direct** | 18-19ms | Same path, from curl |
| **VM start from warm pool** | 1.1-1.4s | Firecracker snapshot restore |
| **TTI (create + first exec)** | ~1.7s | ComputeSDK methodology |
| **Snapshot create (2GB VM)** | 19.4s | Full rootfs + mem dump |
| **Snapshot restore (2GB VM)** | 11.8s | Eager `File` backend |
| **Pool warm (cold, first-time)** | ~60s | Includes Claude pre-warm + golden snapshot |

### Install / deploy

| Metric | Time |
|--------|------|
| Full install from fresh Debian (incl. rootfs build) | 1m 35s |
| — apt-get dependencies | ~20s |
| — debootstrap + chroot setup + Claude install | ~70s |
| — TLS cert + API key + systemd | ~5s |
| Daemon cold start | ~2s |

## Comparison with other providers

Data from [ComputeSDK public leaderboard](https://www.computesdk.com/benchmarks/) (April 16, 2026) + vendor docs.

### TTI (create + first exec) — median

| Provider | Median | Isolation | Deployment |
|----------|--------|-----------|------------|
| Daytona | **120ms** | Docker | Hosted |
| Vercel | 360ms | Firecracker | Hosted |
| E2B | 380ms | Firecracker | Hosted |
| Blaxel | 430ms | — | Hosted |
| Hopx | 1130ms | — | Hosted |
| Modal | 1610ms | gVisor | Hosted |
| **mvm (cloud)** | **~1700ms** | **Firecracker+KVM** | **Self-hosted / local** |
| Cloudflare Sandbox | 1830ms | Container | Hosted |
| CodeSandbox | 2630ms | — | Hosted |

mvm is competitive with the middle tier. The leading providers (Daytona, E2B, Vercel) have dedicated VM pools at scale and can hit sub-500ms TTI.

### Exec round-trip latency (post-start)

| Provider | Latency | Note |
|----------|---------|------|
| **mvm (local daemon)** | **15-16ms** | Same host, vsock |
| mvm (via network) | +RTT | Add your base RTT |
| E2B | 50-200ms | Network RPC |
| Daytona | 50-200ms | Network RPC |
| Sprites | 50-200ms | Network RPC |
| Cloudflare Sandbox | 50-200ms | Network RPC |

**mvm's local-host advantage:** Agents doing N sequential tool calls (e.g., file reads, command runs) save ~35ms × N vs hosted providers. A 50-tool agent saves ~1.75s on exec alone.

### Snapshot / checkpoint

| Provider | Create | Restore | Notes |
|----------|--------|---------|-------|
| Sprites | **~300ms** | **<1s** | COW + NVMe + object storage |
| **mvm (eager File)** | **19.4s** | **11.8s** | Synchronous 2GB mem dump |
| mvm (UFFD, planned) | ~1s | **~30ms** | Lazy page-in, per research |
| E2B | Not public | Not public | — |
| Daytona | Not public | Not public | — |

**mvm's gap:** Current synchronous memory dump is 60× slower than Sprites. UFFD-based lazy restore (research in progress) would close most of this gap — expected 30ms restore vs Sprites' sub-second.

## Where mvm wins

**Local-first execution** (16ms exec). No network provider can beat this. For AI agents doing many sequential tool calls, this compounds to seconds saved per session.

**Same binary, local + cloud.** Develop on macOS via Lima, deploy the same code to bare-metal Linux. E2B, Daytona, Sprites, Cloudflare Sandbox are all cloud-only.

**Free and open source.** Apache 2.0 licensed. Self-host on any Linux box with KVM. No per-second billing, no vendor lock-in.

**Firecracker + KVM isolation.** Same hypervisor as AWS Lambda. Daytona uses Docker, Cloudflare uses containers — weaker isolation.

## Where mvm loses

**Warm start is 10× slower than Daytona.** 1.4s vs 120ms. Daytona uses Docker (weaker isolation tradeoff) and likely pre-allocates more aggressively. Our pool fills to 3 VMs; they may pool hundreds.

**Snapshot operations are slow.** Sprites achieves ~300ms checkpoint with COW + object storage. Our synchronous `File` backend is 60× slower. Mitigation: implement UFFD-based lazy restore (research complete, not yet implemented).

**First exec after pool-claim is slow.** 300-450ms while the VM's memory pages in from snapshot. Stabilizes to 15-16ms after ~3 seconds. UFFD would eliminate this warm-up curve.

## Roadmap

Ordered by impact:

1. **UFFD-based snapshot restore** — expected restore: 12s → <100ms. 400× improvement. Research complete, implementation pending.
2. **Pool pre-allocation tuning** — make pool size configurable per workload; investigate larger default pools on cloud.
3. **TCP timeouts optimization** — remove redundant sudo calls in the daemon's sudo-wrapped commands when already running as root (~100ms per VM start).

## Reproduce these numbers

```bash
# Spin up a test VM (GCP example)
gcloud compute instances create mvm-bench --machine-type=n2-standard-4 \
  --enable-nested-virtualization --image-family=debian-12 \
  --image-project=debian-cloud --boot-disk-size=30GB

# SSH in and install
gcloud compute ssh mvm-bench
sudo bash -c "$(curl -sSL https://get.mvm.dev)"

# Run benchmarks
sudo /usr/local/bin/mvm pool warm
sudo /usr/local/bin/mvm pool status  # wait for 3/3

# TTI test
for i in 1 2 3 4 5; do
  START=$(date +%s%N)
  sudo /usr/local/bin/mvm start t$i
  sudo /usr/local/bin/mvm exec t$i -- echo ok
  END=$(date +%s%N)
  echo "TTI $i: $(( (END - START) / 1000000 ))ms"
  sudo /usr/local/bin/mvm delete t$i --force
done
```
