# Cloud Mode: TCP + TLS + Auth + SDKs

## Problem

mvm is a local-only tool. AI agent frameworks (OpenAI Agents SDK, Claude Agent SDK, LangChain) need programmatic sandbox access on remote servers. Hosted competitors (E2B, Sprites, Cloudflare Sandbox) offer this but require sending code to third-party infrastructure. No product offers the same sandbox API locally and on self-hosted servers.

## Goal

Make mvm work as both a local developer tool and a self-hosted cloud sandbox service with the same API. Ship SDKs in Python, TypeScript, and Go so AI agent frameworks can create and control sandboxes programmatically.

## Architecture

```
Local mode (existing):
  CLI → Unix socket → daemon (inside Lima) → Firecracker VMs

Cloud mode (new):
  SDK/CLI → TCP + TLS → daemon (bare-metal Linux) → Firecracker VMs

Same daemon binary, same API, same VM management.
```

## Hardware Requirements

Cloud mode requires bare-metal Linux with KVM (`/dev/kvm` must be accessible). Firecracker needs direct hardware virtualization — most cloud VMs do not support this.

**Supported providers:** AWS `.metal` instances, GCP with nested virt enabled, Hetzner dedicated servers, OVH bare metal, any bare-metal Linux with KVM.

**Not supported:** Standard cloud VMs (EC2 t3, GCP e2, etc.) unless the provider explicitly enables nested virtualization.

The install script verifies `/dev/kvm` exists before proceeding.

## Scope

### In scope
- TCP listener with TLS support on the daemon
- API key authentication middleware
- CLI remote mode (`--remote`, `MVM_REMOTE` env var)
- Configurable data paths (replace ~50 hardcoded path references)
- Systemd unit file for Linux deployment
- Python SDK (PyPI package)
- TypeScript SDK (npm package)
- Go SDK (public package with duplicated types)
- Install script for bare-metal Linux

### Out of scope
- Multi-tenancy (project namespacing, per-tenant API keys)
- Distributed state (PostgreSQL/etcd backends)
- Docker packaging
- Rate limiting, structured logging (follow-up)
- OAuth/JWT authentication
- Clustering (multiple daemon instances)
- Async SDK support (follow-up)

## Design

### 1. Server: Dual listeners

`internal/server/server.go`

Two `http.Server` instances sharing one `http.ServeMux`:
- **Unix socket server** uses `mux` directly (no auth — local connections are trusted)
- **TCP server** uses `authMiddleware(mux)` (requires API key)

This cleanly separates auth concerns — no need to inspect connection type in middleware.

```go
type ServerConfig struct {
    SocketPath string // Unix socket (local mode)
    ListenAddr string // TCP address, e.g. "0.0.0.0:19876" (cloud mode)
    TLSCert    string // Path to TLS certificate
    TLSKey     string // Path to TLS private key
    APIKey     string // Required for TCP connections
}
```

When both `SocketPath` and `ListenAddr` are set, both servers start in separate goroutines with `errgroup.Group` coordinating shutdown. Either server failing triggers shutdown of both.

TLS is required for TCP unless `MVM_INSECURE=true` is set (for development behind a reverse proxy).

**HTTP timeouts** (TCP server only):
- `ReadTimeout: 30s`
- `WriteTimeout: 5m` (long exec operations)
- `IdleTimeout: 120s`
- Shutdown timeout: 30s

### 2. Auth middleware

`internal/server/auth.go` (new)

```go
func authMiddleware(apiKey string, next http.Handler) http.Handler
```

Checks `Authorization: Bearer <api-key>` on every request except `GET /health` (load balancer probes). Returns 401 with `{"error":"unauthorized"}` on failure.

The API key is set via `--api-key` flag, `MVM_API_KEY` env var, or read from `/etc/mvm/api-key` file (checked in that order). File-based key is preferred for production — env vars are visible via `/proc/*/environ`.

### 3. CLI remote mode

`internal/cli/root.go`, `internal/server/client.go`

New env vars / persistent flags:
```
MVM_REMOTE=https://my-server:19876  (or --remote flag)
MVM_API_KEY=secret-key              (or --api-key flag)
MVM_CA_CERT=/path/to/ca.pem        (for self-signed TLS)
```

When `MVM_REMOTE` is set, `DefaultClient()` returns an HTTP client targeting the remote URL with TLS and auth. All CLI commands work unchanged.

**Self-signed cert trust:** `mvm remote trust <server:port>` fetches and pins the server's certificate (TOFU model, like SSH `known_hosts`). Stored at `~/.mvm/trusted-certs/`. Alternatively, use `MVM_CA_CERT` to specify a CA bundle.

**Interactive exec over TCP+TLS:** `ExecInteractive` needs a dialer abstraction — currently hardcodes `net.DialTimeout("unix", ...)`. Add a `dialFunc` to the client that returns either a Unix conn or a `tls.Conn` depending on mode. The raw HTTP upgrade request then goes over the TLS connection transparently.

### 4. `serve start` flags

```
mvm serve start                                          # Unix socket only (local)
mvm serve start --listen 0.0.0.0:19876                   # + TCP (insecure, dev only)
mvm serve start --listen 0.0.0.0:19876 \
    --tls-cert /etc/mvm/cert.pem \
    --tls-key /etc/mvm/key.pem \
    --api-key-file /etc/mvm/api-key                      # Full cloud mode
```

### 5. Configurable paths

`internal/firecracker/config.go`

Replace all hardcoded path constants with functions. Full list of constants to migrate:

| Constant | Current value | References |
|----------|--------------|------------|
| `CacheDir` | `/opt/mvm/cache` | 9 sites |
| `VMsDir` | `/opt/mvm/vms` | 1 site (via VMDir) |
| `KeyDir` | `/opt/mvm/keys` | 3 sites |
| `RunDir` | `/run/mvm` | 12+ sites |
| `PoolDir` | `/opt/mvm/pool` | 3 sites |
| `poolSnapshotDir` | `/opt/mvm/pool/snapshot` | 8 sites |
| `snapshotsBaseDir` (routes.go) | `/opt/mvm/snapshots` | 6 sites |

~50 call sites across config.go, pool.go, routes.go, init.go, install.go, snapshot.go, and their tests.

```go
func DataDir() string {
    if d := os.Getenv("MVM_DATA_DIR"); d != "" { return d }
    return "/opt/mvm"
}
```

Also rename `IsInsideLima()` to `IsLinux()` — it returns `runtime.GOOS == "linux"` and the name is misleading when running on cloud servers.

### 6. Systemd unit + install script

`deploy/mvm-daemon.service`:
```ini
[Unit]
Description=mvm sandbox daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/mvm serve start --listen 0.0.0.0:19876 --tls-cert /etc/mvm/cert.pem --tls-key /etc/mvm/key.pem --api-key-file /etc/mvm/api-key
Environment=MVM_DATA_DIR=/var/mvm
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`scripts/install-cloud.sh`:
1. Check `/dev/kvm` exists — fail with clear error if not
2. Download mvm binary + Firecracker for linux/arm64 or linux/amd64
3. Create `/var/mvm/{cache,vms,keys,pool}` and `/etc/mvm/`
4. Generate self-signed TLS cert at `/etc/mvm/cert.pem`
5. Generate random API key at `/etc/mvm/api-key` (0600, root-only)
6. Install systemd unit, enable, start
7. Print: remote URL, API key, `mvm remote trust` command

### 7. Python SDK

Package: verify `mvm-sandbox` on PyPI (fallback: `agentstep-mvm`)

```python
from mvm import Sandbox

client = Sandbox.connect("https://my-server:19876", api_key="secret")
# Or local: client = Sandbox.connect()

vm = client.create("my-sandbox", cpus=2, memory_mb=512)
result = vm.exec("echo hello")
print(result.output)     # "hello\n"
print(result.exit_code)  # 0

vm.snapshot("before-install")
vm.exec("pip install pandas")
vm.restore("before-install")

vm.stop()
vm.delete()
```

MVP implementation: `httpx` client, ~400 lines. Includes error hierarchy, type hints, custom CA cert support via `verify` parameter. Excludes async support, retries, streaming (v2).

### 8. TypeScript SDK

Package: verify `@agentstep/mvm-sdk` on npm

```typescript
import { Sandbox } from '@agentstep/mvm-sdk';

const client = new Sandbox({
  remote: 'https://my-server:19876',
  apiKey: 'secret',
});

const vm = await client.create('my-sandbox', { cpus: 2, memoryMb: 512 });
const result = await vm.exec('echo hello');
console.log(result.output);  // "hello\n"

await vm.snapshot('checkpoint-1');
await vm.exec('npm install express');
await vm.restore('checkpoint-1');

await vm.delete();
```

MVP implementation: `fetch`-based, ~400 lines. Full TypeScript types for all request/response shapes, error classes, custom CA via `https.Agent`. Excludes AbortController, streaming (v2).

### 9. Go SDK

Package: `github.com/agentstep/mvm/sdk`

**Not a simple extraction** — `internal/server/client.go` imports internal types. The SDK duplicates all request/response types (~70 lines) to avoid coupling to server internals. `ExecInteractive` is excluded from the SDK (raw PTY relay is an edge case for programmatic clients).

```go
import "github.com/agentstep/mvm/sdk"

client := sdk.New("https://my-server:19876", sdk.WithAPIKey("secret"))
vm, _ := client.CreateVM(ctx, sdk.CreateVMRequest{Name: "sandbox", Cpus: 2})
result, _ := client.Exec(ctx, "sandbox", "echo hello")
client.DeleteVM(ctx, "sandbox")
```

~400 lines including type definitions.

## API Reference

All existing endpoints, unchanged. Auth required on TCP; not on Unix socket.

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | /health | No | Health check |
| GET | /vms | Yes | List VMs |
| POST | /vms | Yes | Create VM |
| POST | /vms/{name}/exec | Yes | Execute command |
| POST | /vms/{name}/stop | Yes | Stop VM |
| DELETE | /vms/{name} | Yes | Delete VM |
| POST | /vms/{name}/pause | Yes | Pause VM |
| POST | /vms/{name}/resume | Yes | Resume VM |
| POST | /vms/{name}/snapshot | Yes | Create snapshot |
| POST | /vms/{name}/restore | Yes | Restore snapshot |
| GET | /snapshots | Yes | List snapshots |
| DELETE | /snapshots/{name} | Yes | Delete snapshot |
| POST | /build | Yes | Build custom image |
| GET | /images | Yes | List images |
| DELETE | /images/{name} | Yes | Delete image |
| GET | /pool | Yes | Pool status |
| POST | /pool/warm | Yes | Warm pool |

## Verification

### Cloud deployment test
```bash
# On a Linux server with KVM:
curl -sSL https://get.mvm.dev | bash
cat /etc/mvm/api-key  # auto-generated

# From laptop:
mvm remote trust my-server:19876
export MVM_REMOTE=https://my-server:19876
export MVM_API_KEY=$(ssh root@my-server cat /etc/mvm/api-key)
mvm pool status
mvm start test1
mvm exec test1 -- uname -a
mvm delete test1 --force
```

### SDK test (Python)
```python
from mvm import Sandbox
client = Sandbox.connect("https://server:19876", api_key="key")
vm = client.create("sdk-test")
assert vm.exec("echo works").output.strip() == "works"
vm.delete()
```

### SDK test (TypeScript)
```typescript
import { Sandbox } from '@agentstep/mvm-sdk';
const client = new Sandbox({ remote: 'https://server:19876', apiKey: 'key' });
const vm = await client.create('sdk-test');
const r = await vm.exec('echo works');
assert(r.output.trim() === 'works');
await vm.delete();
```
