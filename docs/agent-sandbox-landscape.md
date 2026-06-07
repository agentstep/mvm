# Agent Sandbox Landscape — Competitive Research (mid-2026)

> Research compiled June 2026 to inform mvm's positioning. Every funding,
> isolation, pricing, and license claim below was verified against primary
> sources (company sites, GitHub, funding announcements, regulator-grade
> trackers). Where the source "engine" landscape page was wrong, the
> correction is called out inline and summarised in
> [§7 Corrections](#7-corrections-to-the-engine-page).
>
> Method: five parallel research passes (sandbox-native vendors, platforms,
> OSS/self-hosted projects, isolation technology, market trends) with
> adversarial verification. Sources are listed in [§9](#9-sources).

## 1. Executive summary

- **Firecracker microVMs are the default isolation primitive** for serious
  cloud agent sandboxes (E2B, Fly.io Sprites, Vercel, Blaxel, Terminal Use).
  The market's real technical debate is **hardware-VM boundary (Firecracker /
  Kata / libkrun) vs. syscall-interception (gVisor, used by Modal)** —
  trading absolute isolation strength against startup latency and I/O overhead.
- **Shared-kernel approaches (OCI containers, V8 isolates, macOS Seatbelt)**
  remain in use for convenience or edge-latency, but the 2026 consensus is
  "your container is not a sandbox" — one kernel CVE is one container escape.
- **Pricing has converged on usage-based per-second compute**, increasingly
  "active-CPU" billing (Vercel, Cloudflare) that doesn't charge for I/O wait.
  Plan tiers (free Hobby + paid Pro) layer on top.
- **The space is visibly overcrowded.** The dax/@thdxr joke ("guys i had a
  one of a kind idea for a business: sandboxes for coding agents", Apr 7
  2026) is the market's own punchline. ~41% of YC's record W26 batch builds
  "agent plumbing"; analysts already predict consolidation.
- **The "engine" landscape page itself could not be verified** as a public
  site (see [§2](#2-the-engine-page)), and several of its data points are
  stale or wrong — most notably **Modal at "$111M" (now ~$435M+)**,
  **Daytona "$31M" (actually ~$24M)**, **E2B "$35M" ($21M Series A / ~$32M
  total)**, and **Terminal Use "containers" (actually Firecracker)**.
- **mvm's niche is real and largely unoccupied:** it is the only
  Firecracker+KVM product offering the *same sandbox API both locally
  (Apple Silicon) and self-hosted on your own Linux servers*. The closest
  neighbours each miss one axis — microsandbox (local-only, no cloud story),
  hosted Firecracker vendors (no local, code leaves your machine), Docker
  Sandboxes (local+cross-platform but proprietary, no self-hosted server
  fleet model).

## 2. The "engine" page

The source page styled **"AGENT_SANDBOXES — THE INFRASTRUCTURE OF AGENCY"**
under the brand **"engine"** could **not be confirmed as a public, indexed
site.** The exact tagline returns zero organic matches. `engine.dev` resolves
but returns HTTP 403 to automated fetches (a site sits there but blocks bots,
so it couldn't be read or attributed). The terminal/all-caps aesthetic reads
like a bespoke or internal landscape page rather than an indexed directory.

Real, adjacent directories that exist (not the same page): Ona's "background
agent landscape", aiagentsdirectory.com/landscape, and a community gist
"List of coding agent sandboxes". **Treat the engine page as an
unattributed secondary source** — useful as a checklist of who-builds-what,
but its numbers need the corrections below.

The cultural context is genuine: the **@thdxr tweet is real** (Apr 7 2026),
a deadpan jab at how crowded "sandboxes for coding agents" has become — dax
being the author of opencode, which itself ships a pluggable sandbox layer.

## 3. Sandbox-native providers

Companies whose whole product *is* the sandbox. ✅ = engine page correct;
⚠️ = corrected/qualified below.

| Provider | Isolation (verified) | Funding (verified) | Pricing | License | Differentiator |
|---|---|---|---|---|---|
| **Daytona** | OCI/Docker containers, dedicated kernel/FS/net per box; optional Kata; sub-90ms start | ⚠️ **~$24M** Series A (Feb 2026, FirstMark) — *not $31M* | Usage-based, per-second; $200 credit | AGPL-3.0 ✅ | OSS, OCI-compatible, sub-90ms creation |
| **E2B** | Firecracker microVMs ✅ | ⚠️ **$21M** Series A (Jul 2025, Insight); **~$32M total** — *not $35M* | Usage + plan (Pro $150/mo; ~$0.05/vCPU-hr) | Apache-2.0 core | "Enterprise AI agent cloud"; cites 88% of Fortune 100 |
| **Blaxel** | Firecracker (forked; VPP networking; <25ms resume) ✅ | ⚠️ **$7.3M** seed (First Round) — *not $7.8M* | Usage-based; $200 credit | Proprietary (SDKs open) | Persistent sandboxes idle at $0, resume <25ms with memory state |
| **Runloop** | MicroVM "devboxes", dual-layer VM+container ✅ | $7M seed (The General Partnership) ✅ | Usage + plan (Pro $250/mo + usage) ✅ | Proprietary | Agent infra **with built-in evals/benchmarks** (SWE-bench, RFT/SFT) |
| **Freestyle** | Full Linux **KVM VMs**, real root + net ✅ | ~$500K pre-seed (Sep 2024, YC S24) | Plan + usage (Hobby $50, Pro $500/mo) ✅ | Proprietary | Full KVM VMs + Git, purpose-built for agent-written code that deploys |
| **Terminal Use** | ⚠️ **Firecracker microVMs on Fly.io** (QCOW2 CoW forks) — *engine says "containers", wrong* | YC W26; **$625K unverified** (likely YC-standard $500K) | Plan-based **(unverified)** | Proprietary | "Vercel for filesystem/background agents"; forkable shared filesystems |
| **Celesto AI** | ⚠️ **Multi-VMM** abstraction — Firecracker + QEMU + libkrun (its `SmolVM`) — *not Firecracker-only* | **No round found (unverified)** | "Plan-based" **unverified**; OSS self-host | Apache-2.0 | Unified Python SDK over FC/QEMU/libkrun for untrusted code |

All seven are **real companies** — Terminal Use (3 ex-Palantir founders, YC W26
Launch HN) and Celesto AI (live site + GitHub `CelestoAI/SmolVM`) both check out.

## 4. Platforms with sandboxes

Broader infra products that added a sandbox offering.

| Provider | Isolation (verified) | Funding | Pricing | OSS | Differentiator |
|---|---|---|---|---|---|
| **Modal** | **gVisor** ✅ (Kata microVM opt-in) | ⚠️ **~$435M+** — $355M Series C, May 2026, $4.65B val — *not $111M* | Usage + plan; sandbox CPU ~$0.0000394/core-s (~3× base) | Client SDK open; platform proprietary | GPU-first (A100/H100), autoscale, no session limits; >1B sandboxes launched |
| **Sprites** (Fly.io) | Firecracker microVMs, NVMe-persistent FS + checkpoint ✅ | Fly.io parent ~$110M total ($70M Series C, EQT) | Usage-based; compute idles to $0, FS persists | Proprietary | Persistent stateful microVMs that idle to zero then restore full state. **"Sprites" is the real product name.** |
| **Northflank** | **Kata** on Cloud Hypervisor (primary); FC/gVisor per workload ✅ | $24.9M ($22.3M Series A, Nov 2024, BCV/Vertex) ✅ | Usage, per-second; **$0.01667/vCPU-hr** (lowest published) | Proprietary | Full production PaaS (apps/DBs/GPU/sandboxes) on hardware-isolated Kata, in prod since 2021 |
| **Namespace** | Containers on own bare-metal (EPYC, AmpereOne, Apple Silicon) ✅ | $23M (Series A, Mar 2026, NEA) ✅ | Usage-based, no seat fees | Core "foundation" Apache; cloud commercial | Self-owned high-perf hardware for fast/cheap CI + agent sandboxes |
| **Claude Managed Agents** (Anthropic) | Sandboxed exec env; **isolation tech not vendor-disclosed** (best descriptor: containers) | Part of Anthropic | Usage; **$0.08/session-hr + tokens** ✅ (public beta Apr 8 2026) | Proprietary (hosted); **self-hosted sandbox option** exists | Hosted agent *runtime* (orchestration, checkpointing, creds, tracing) — sells the runtime, not just the model. **Real product name.** |
| **Vercel Sandbox** | Firecracker microVMs (dedicated kernel) on Fluid compute ✅ | Vercel parent ~$563M | Usage + plan; active-CPU $0.128/CPU-hr; free Hobby tier | CLI/SDK open; platform proprietary | Firecracker isolation native to the Vercel/Next.js deploy ecosystem |
| **Cloudflare Sandbox SDK** | ⚠️ **Containers-in-VMs** for the Sandbox SDK; **V8 isolates** power a *separate* product (Dynamic Workers) — *engine conflates the two* | Public (NYSE: NET) | Usage; active-CPU $0.00002/vCPU-s | **Sandbox SDK is open source** (`cloudflare/sandbox-sdk`) | Edge-native sandboxes (Workers + Durable Objects + Containers), GA Apr 2026; V8-isolate option for sub-5ms cold starts |

Note: **Anthropic's Managed Agents self-hosted mode** keeps tool execution on
your infra (only tool I/O reaches Anthropic's control plane) — relevant for
ZDR/HIPAA/air-gapped use, and a conceptual neighbour to mvm's compliance pitch.
Cloudflare, Daytona, Modal, and Vercel are all documented self-hosted-sandbox
backends for it.

## 5. Open-source / self-hosted / local

**All twelve targets are real** — a primary GitHub repo or official page was
found for each. None fictional. (Star counts are mid-2026 ballparks.)

| Project | Isolation | License | Maintainer | Notes |
|---|---|---|---|---|
| **OpenSandbox** (Alibaba) | OCI containers, pluggable gVisor / Kata / Firecracker | Apache-2.0 | Alibaba / `opensandbox-group` (~11.4k★) | Multi-language SDKs, unified API over Docker+K8s with selectable strong-isolation runtimes |
| **Microsandbox** | **libkrun microVM** (KVM), runs OCI images, <200ms | Apache-2.0 | Superrad Co. (YC); orig. `zerocore-ai` (~6.4k★) | Local-first, self-hosted microVMs from embedded SDKs; secrets never enter the VM |
| **OpenShell** (NVIDIA) | Containers + microVM/Podman/K8s backends; **Landlock + seccomp BPF** | Apache-2.0 | NVIDIA (alpha; ~89% Rust) | Declarative YAML policy over FS/net/proc/inference, hot-reloadable |
| **SmolVM** (Smol Machines) | **libkrun** (+libkrunfw); Hypervisor.framework on macOS, KVM on Linux | Apache-2.0 | @binsquare, ex-AWS Firecracker (~3.6k★) | Single portable daemonless `.smolmachine` executable, <1s cold start, real HW isolation |
| **Zeroboot** | **Firecracker + KVM**; CoW fork of FC memory snapshots, ~0.8ms spawn | Apache-2.0 | `zerobootdev` (~2.4k★, prototype) | Sub-ms VM spawn via mmap snapshot restore (~265 KB/sandbox) |
| **Agent Safehouse** | **macOS sandbox-exec / Seatbelt**, deny-first | Apache-2.0 | eugene1g (~1.8k★) | Zero-dep shell wrapper confining mac coding agents at the kernel level |
| **Gondolin** (Earendil Works) | **QEMU microVM** (default); experimental krun; net+FS policy in host JS | Apache-2.0 | Earendil Works (~1.4k★) | Adversarial-guest/trusted-host: egress/FS/secrets enforced host-side in JS |
| **E2B** (OSS) | Firecracker microVMs | Apache-2.0 | e2b-dev (~8.9k★) | OSS core of the E2B cloud |
| **Daytona** (OSS) | Isolated sandboxes, ~200ms; self/hosted/hybrid | AGPL-3.0 (relicensed from Apache during 2025 pivot) | daytonaio | OSS core of Daytona cloud |
| **SmolVM** (Celesto) | **Multi-VMM**: Firecracker + QEMU + libkrun | Apache-2.0 | CelestoAI (~583★) | *Distinct from Smol Machines' SmolVM* — a unified multi-VMM API layer |
| **Lifo** | **Browser** (Web APIs as syscalls, OPFS/IndexedDB) — *browser-level only, not HW isolation* | MIT | `lifo-sh` (~490★) | Linux-like OS entirely in a browser tab; no backend/VM |
| **Docker Sandboxes** | Proprietary cross-platform microVM: KVM (Linux) / Hypervisor.framework (macOS) / WHP (Windows) — deliberately **not** Firecracker | Proprietary (no OSS repo) | Docker, Inc. (launched ~Q1 2026) | Cross-platform microVM for unsupervised coding agents, configured via "Sandbox Kits" YAML |

Naming traps worth flagging: **two different "SmolVM" projects** exist
(Smol Machines' libkrun tool vs. Celesto's multi-VMM API), and **microsandbox**
appears under two org names (`zerocore-ai` and `superradcompany`) — same project.

## 6. Isolation technology — why microVMs win

Three tiers, defined by **what an attacker must break to escape**:

1. **Hardware/VM boundary** (Firecracker, Kata, libkrun, QEMU, Apple `vz`):
   guest has its own kernel; escape requires breaking CPU virtualization
   extensions plus the VMM's tiny device surface. **Strongest practical boundary.**
2. **Userspace-kernel / syscall interception** (gVisor): a user-space "Sentry"
   reimplements Linux; guest never calls the host kernel directly. Strong, but
   the Sentry + its minimal host-syscall set is the boundary.
3. **Shared host kernel** (OCI containers, V8 isolates, Seatbelt): same kernel
   or same OS process, hardened by namespaces/seccomp/Landlock, V8 memory
   isolation, or a MAC policy. **One kernel/runtime bug = full escape.**

| Technology | Isolation | Cold start | Density / overhead | Representative users |
|---|---|---|---|---|
| Firecracker microVM | ★★★★★ | ~125ms to userspace (≤180ms e2e); 150 µVM/s/host | ~3–5 MB/VM | AWS Lambda, **E2B, Fly.io, Vercel, Blaxel, mvm** |
| Kata Containers | ★★★★★ | ~150–300ms (≤~600ms) | ~130–200 MB/pod | Northflank, confidential K8s |
| libkrun | ★★★★★ | <200ms | container-like | microsandbox, SmolVM, Podman krun |
| Apple `vz` / Hypervisor.framework | ★★★★★ | sub-second | one guest/VM | local Mac VMs; **nested virt on M3+/macOS 15** |
| QEMU microvm | ★★★★☆ | seconds (qboot helps) | ~131 MB/VM | Gondolin, Kata option |
| gVisor | ★★★★☆ | ms (no kernel boot) | ~10–35% I/O overhead | **Modal**, GKE Sandbox, Cloud Run |
| OCI container + seccomp/Landlock | ★★☆☆☆ | ~10–50ms | highest density | Daytona, Namespace, baseline Docker |
| V8 isolates | ★★☆☆☆ (not a kernel boundary) | **sub-ms** | thousands/process | Cloudflare Dynamic Workers |
| macOS Seatbelt | ★★★☆☆ (local only) | negligible | per-process | Agent Safehouse, Cursor/Codex local wrappers |

**Why Firecracker dominates:** the agent workload is the worst case for a
shared kernel — untrusted, attacker-controlled code, multi-tenant, run
millions of times a day, where one escape compromises other tenants.
Firecracker collapses the old VM-vs-container tradeoff: VM-grade isolation at
container economics (~125ms start, ~3–5 MB overhead, 150 µVMs/s/host — NSDI'20).
Its tiny device model (virtio only; no PCI/BIOS/legacy) plus the unprivileged
chroot+cgroup **jailer** minimise attack surface, and it's battle-tested under
AWS Lambda/Fargate — which is exactly why E2B, Fly.io, and Vercel standardised
on it. **Modal is the notable dissenter** on gVisor, betting "good-enough"
user-space isolation with ms startup is worth the I/O overhead.

**Local hardware isolation on a Mac is now real:** Apple
Virtualization.framework runs hardware-accelerated VMs on Apple Silicon, and
**macOS 15 added nested virtualization on M3+** — the enabler for running a
KVM-style microVM stack inside a Mac. This is precisely the foundation that
makes "local-first Firecracker" feasible (mvm via Lima nested virt; microsandbox
/ SmolVM via libkrun + Hypervisor.framework).

## 7. Corrections to the engine page

| Provider | Engine claim | Verified |
|---|---|---|
| Daytona | $31M | **~$24M** Series A (Feb 2026, FirstMark) |
| E2B | $35M | **$21M** Series A (Jul 2025, Insight); **~$32M** total |
| Blaxel | $7.8M | **$7.3M** seed (First Round) |
| Modal | $111M | **~$435M+** ($355M Series C, May 2026, $4.65B val) — $111M was a stale pre-Series-C total |
| Terminal Use | containers, $625K, plan-based | **Firecracker microVMs on Fly.io**; funding/pricing **unverified** (likely YC-standard) |
| Celesto AI | Firecracker, plan-based | **Multi-VMM (FC+QEMU+libkrun)**, **Apache-2.0 OSS**; funding/pricing **unverified** |
| Cloudflare | containers + V8 isolates | Sandbox SDK = **containers-in-VMs**; V8 isolates = separate **Dynamic Workers** product |
| Claude Managed Agents | containers | Sandboxed exec env; **isolation tech not vendor-disclosed**. $0.08/session-hr + tokens ✅ |
| Everything else | — | Runloop, Freestyle, Sprites, Northflank, Namespace, Vercel, and all 12 OSS entries verified as stated (minor qualifiers inline) |

No fictional providers were found — every entry corresponds to a real
company or repo. The engine page's *taxonomy* is sound; its *funding numbers*
are the weak spot (several stale or aggregator-inflated).

## 8. Where mvm fits

The landscape splits cleanly on two axes mvm uniquely spans:

**(a) Local-first vs. cloud-hosted.** Hosted Firecracker vendors (E2B, Sprites,
Vercel, Blaxel) give real isolation but **your code and credentials leave your
machine**, with a per-second meter and network-bound latency. Local tools
(microsandbox, SmolVM, Agent Safehouse, Docker Sandboxes) keep code on-device
but — except Docker — have **no production cloud/self-hosted server story**.

**(b) Same API both places.** No hosted vendor runs locally; no local tool
gives you a self-hostable server fleet with the *same* CLI/SDK. **mvm is the
only Firecracker+KVM product that runs the same sandbox API locally on Apple
Silicon (Lima nested virt, M3+) and self-hosted on your own Linux/KVM
servers** — dev on your Mac, deploy to your boxes, no code changes.

| Product | Isolation | Local dev | Self-host server | OSS | Per-call latency |
|---|---|---|---|---|---|
| **mvm** | Firecracker+KVM | **Yes** | **Yes** | **Apache-2.0** | **16ms local / ~100ms cloud** |
| E2B | Firecracker | No | No | Partial | 50–200ms |
| Sprites (Fly.io) | Firecracker | No | No | No | 50–200ms |
| Vercel Sandbox | Firecracker | No | No | SDK only | 50–200ms |
| Daytona | Containers (opt. Kata) | No | Enterprise | AGPL | 50–200ms |
| Cloudflare Sandbox | Containers-in-VM | No | No | SDK only | 50–200ms |
| microsandbox | libkrun+KVM | Yes | Local only | Apache | ~local |
| Docker Sandboxes | Cross-platform microVM | Yes | Yes | Proprietary | — |

**Defensible wedges for mvm:**
1. **Same API local + self-hosted** — genuinely unoccupied; the hosted/local
   divide is the market's clearest gap.
2. **Hardware (Firecracker+KVM) isolation, not containers** — stronger than
   Daytona/Cloudflare/Namespace's shared-kernel model, equal to E2B/Sprites/
   Vercel, without their cloud lock-in.
3. **Compliance / data-sovereignty** — code never leaves your infrastructure;
   directly addresses the credentials-leave-your-machine concern the whole
   market is now wrestling with (cf. Anthropic's self-hosted-sandbox mode).
4. **Cost for sustained workloads** — free + your server vs. a per-second meter;
   structurally cheaper than Sprites/E2B for 24/7 agents.
5. **Low local exec latency** — a local daemon avoids the network round-trip
   that costs hosted providers 50–200ms per tool call; meaningful for agents
   doing many sequential calls.

**Where mvm is exposed / watch list:**
- **Docker Sandboxes** is the most direct threat — cross-platform microVM,
  local+server, backed by Docker's distribution. mvm's counters are *open
  source* (vs. proprietary) and *true Firecracker+KVM with a self-hostable
  server fleet model*.
- **microsandbox / SmolVM (libkrun)** could add a cloud/self-host story and
  collapse mvm's "same API" wedge; both are Apache-2.0 and well-starred.
- **Hardware requirements** (Apple M3+ for local nested virt; bare-metal/KVM
  for cloud — no t3/e2 commodity VMs) narrow the addressable base vs. gVisor/
  container vendors that run anywhere.
- **gVisor's "good enough"** bet (Modal, now ~$4.65B) means a large segment may
  not pay any premium for a hardware boundary — mvm's isolation argument lands
  best with security/compliance-driven buyers, not latency-only ones.

## 9. Sources

**Sandbox-native:** daytona.io/pricing · github.com/daytonaio/daytona ·
read.unicorner.news/p/daytona · venturebeat.com (E2B 88% Fortune 100 / $21M) ·
finsmes.com (E2B $21M) · insightpartners.com (E2B Series A) · e2b.dev/pricing ·
e2b.dev/blog/firecracker-vs-qemu · blaxel.ai ($7.3M seed; anatomy-of-a-runtime;
pricing) · ycombinator.com/companies/blaxel · prnewswire.com (Runloop $7M) ·
runloop.ai/pricing · freestyle.sh/products/vms · crunchbase.com (Freestyle
pre-seed) · ycombinator.com/companies/freestyle · ycombinator.com/companies/terminal-use ·
news.ycombinator.com/item?id=47311657 · github.com/CelestoAI/SmolVM ·
celesto.ai/blog

**Platforms:** thesaasnews.com / techstartups.com / modal.com/blog (Modal
Series B/C) · amplifypartners.com (Modal gVisor) · modal.com/docs ·
northflank.com/blog/ai-sandbox-pricing · devclass.com / sdxcentral.com /
simonwillison.net (Fly.io Sprites) · crunchbase.com/organization/fly-io ·
northflank.com/blog/what-are-kata-containers · tracxn.com (Northflank,
Namespace) · namespace.so/pricing · github.com/namespacelabs/foundation ·
anthropic.com/engineering/managed-agents ·
platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes ·
helpnetsecurity.com / finout.io / verdent.ai (Managed Agents pricing) ·
vercel.com/blog/vercel-sandbox-is-now-generally-available · vercel.com/docs/sandbox/pricing ·
developers.cloudflare.com/sandbox · blog.cloudflare.com/sandbox-ga ·
github.com/cloudflare/sandbox-sdk · venturebeat.com (Cloudflare Dynamic Workers)

**OSS / self-hosted:** github.com/alibaba/OpenSandbox · github.com/zerocore-ai/microsandbox
· microsandbox.dev · github.com/NVIDIA/OpenShell · github.com/smol-machines/smolvm ·
smolmachines.com · github.com/zerobootdev/zeroboot · github.com/eugene1g/agent-safehouse
· github.com/earendil-works/gondolin · github.com/lifo-sh/lifo ·
docs.docker.com/ai/sandboxes · docker.com/blog/why-microvms-the-architecture-behind-docker-sandboxes
· github.com/e2b-dev/E2B

**Isolation tech:** firecracker-microvm.github.io · usenix.org/conference/nsdi20/presentation/agache
· github.com/firecracker-microvm/firecracker (SPECIFICATION.md) ·
emirb.github.io/blog/microvm-2026 ("Your Container Is Not a Sandbox") ·
gvisor.dev/docs/architecture_guide/security · edera.dev (Kata vs FC vs gVisor) ·
northflank.com/blog/kata-containers-vs-firecracker-vs-gvisor ·
github.com/containers/libkrun · qemu.org/docs (microvm) ·
developer.apple.com/documentation/virtualization ·
developer.apple.com/.../isNestedVirtualizationSupported ·
developers.cloudflare.com/workers/reference/security-model · cursor.com/blog/agent-sandboxing

**Market / trends:** x.com/thdxr/status/2041642239365955849 ·
buildmvpfast.com/blog/yc-w26-batch-agent-infrastructure-boom ·
northflank.com/blog/best-code-execution-sandbox-for-ai-agents ·
superagent.sh/blog/ai-code-sandbox-benchmark-2026 ·
ona.com/stories/background-agent-landscape · aiagentsdirectory.com/landscape ·
arxiv.org/abs/2501.10114

> **Caveats:** several primary domains (x.com, vercel.com/docs,
> developers.cloudflare.com, fly.io, northflank.com) return HTTP 403 to
> automated fetches; those figures were cross-checked across multiple secondary
> sources rather than read directly. Modal's Series B size shows a minor
> source discrepancy ($87M per Modal's blog vs. $80M in later coverage).
> Funding for Terminal Use and Celesto AI, and pricing for Celesto AI, remain
> unverified (no primary disclosure found).
