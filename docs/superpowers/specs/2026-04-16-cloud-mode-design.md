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

## Scope

### In scope
- TCP listener with TLS support on the daemon
- API key authentication middleware
- CLI remote mode (`--remote`, `MVM_REMOTE` env var)
- Configurable data paths (replace hardcoded `/opt/mvm/`)
- Systemd unit file for Linux deployment
- Python SDK (PyPI package)
- TypeScript SDK (npm package)
- Go SDK (extracted from internal/server/client.go)
- Install script for bare-metal Linux

### Out of scope
- Multi-tenancy (project namespacing, per-tenant API keys)
- Distributed state (PostgreSQL/etcd backends)
- Docker packaging
- Rate limiting
- OAuth/JWT authentication
- Clustering (multiple daemon instances)

## Design

### 1. Server: TCP + TLS listener

`internal/server/server.go`

Add a `ListenAddr` field to `ServerConfig`. When set, the daemon listens on TCP instead of (or in addition to) the Unix socket.

```go
type ServerConfig struct {
    SocketPath string // Unix socket (local mode)
    ListenAddr string // TCP address, e.g. "0.0.0.0:19876" (cloud mode)
    TLSCert    string // Path to TLS certificate
    TLSKey     string // Path to TLS private key
    APIKey     string // Required for TCP connections
}
```

When both `SocketPath` and `ListenAddr` are set, the daemon serves on both (Unix for local CLI, TCP for remote clients). Unix socket connections skip auth.

TLS is required for TCP — plaintext TCP is rejected unless `MVM_INSECURE=true` is explicitly set (for development/testing behind a reverse proxy).

### 2. Auth middleware

`internal/server/auth.go` (new)

HTTP middleware that checks `Authorization: Bearer <api-key>` on every request. Skips auth for:
- Unix socket connections (local CLI, already trusted)
- `GET /health` (load balancer probes)

The API key is a static string set via `--api-key` flag or `MVM_API_KEY` env var. No key rotation, no scoping — simple is correct for single-tenant.

### 3. CLI remote mode

`internal/cli/root.go`, `internal/server/client.go`

Two new env vars / flags:
```
MVM_REMOTE=https://my-server:19876  (or --remote flag)
MVM_API_KEY=secret-key              (or --api-key flag)
```

When `MVM_REMOTE` is set, `DefaultClient()` returns an HTTP client targeting the remote URL instead of the Unix socket. All CLI commands work unchanged — they call the same daemon API.

The client adds `Authorization: Bearer <key>` to every request when `MVM_API_KEY` is set.

For TLS, the client uses the system CA pool by default. Custom CA via `MVM_CA_CERT` env var.

### 4. Configurable paths

`internal/firecracker/config.go`

Replace constants with functions that read env vars with defaults:

```go
func DataDir() string {
    if d := os.Getenv("MVM_DATA_DIR"); d != "" { return d }
    return "/opt/mvm"
}
func CacheDir() string { return filepath.Join(DataDir(), "cache") }
func VMsDir() string   { return filepath.Join(DataDir(), "vms") }
func KeysDir() string  { return filepath.Join(DataDir(), "keys") }
```

Update all references from constants to function calls.

### 5. Systemd unit + install script

`deploy/mvm-daemon.service`:
```ini
[Unit]
Description=mvm sandbox daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/mvm serve start --listen 0.0.0.0:19876
Environment=MVM_DATA_DIR=/var/mvm
Environment=MVM_API_KEY=change-me
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`scripts/install-cloud.sh`: Downloads mvm binary + Firecracker, creates data dirs, installs systemd unit, generates TLS cert (self-signed), generates random API key, enables and starts the service.

### 6. Python SDK

Package: `mvm` on PyPI

```python
from mvm import Sandbox

# Connect to remote
client = Sandbox.connect("https://my-server:19876", api_key="secret")

# Or local (auto-detects Unix socket)
client = Sandbox.connect()

# Create and use
vm = client.create("my-sandbox", cpus=2, memory_mb=512)
result = vm.exec("echo hello")
print(result.output)     # "hello\n"
print(result.exit_code)  # 0

# Interactive
vm.exec_interactive("bash")  # Opens PTY

# Checkpoint
vm.snapshot("before-install")
vm.exec("pip install pandas")
vm.restore("before-install")  # Reverts to pre-install state

# Custom image
client.build("Dockerfile", tag="with-postgres")
vm = client.create("db-sandbox", image="with-postgres")

# Cleanup
vm.stop()
vm.delete()
```

Implementation: thin HTTP client using `requests` or `httpx`. ~300 lines. Maps 1:1 to the REST API.

### 7. TypeScript SDK

Package: `@mvm/sdk` on npm

```typescript
import { Sandbox } from '@mvm/sdk';

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

Implementation: thin HTTP client using `fetch`. ~300 lines. Same API shape as Python.

### 8. Go SDK

Package: `github.com/agentstep/mvm/sdk`

Extract `internal/server/client.go` into a public `sdk/` package. The Go client already has all the methods — just make it importable:

```go
import "github.com/agentstep/mvm/sdk"

client := sdk.New("https://my-server:19876", sdk.WithAPIKey("secret"))
vm, _ := client.CreateVM(ctx, sdk.CreateVMRequest{Name: "sandbox", Cpus: 2})
result, _ := client.Exec(ctx, "sandbox", "echo hello")
client.DeleteVM(ctx, "sandbox")
```

## API Reference

All existing endpoints, unchanged:

| Method | Path | Description |
|--------|------|-------------|
| GET | /health | Health check (no auth) |
| GET | /vms | List VMs |
| POST | /vms | Create VM |
| POST | /vms/{name}/exec | Execute command |
| POST | /vms/{name}/stop | Stop VM |
| DELETE | /vms/{name} | Delete VM |
| POST | /vms/{name}/pause | Pause VM |
| POST | /vms/{name}/resume | Resume VM |
| POST | /vms/{name}/snapshot | Create snapshot |
| POST | /vms/{name}/restore | Restore snapshot |
| GET | /snapshots | List snapshots |
| DELETE | /snapshots/{name} | Delete snapshot |
| POST | /build | Build custom image |
| GET | /images | List images |
| DELETE | /images/{name} | Delete image |
| GET | /pool | Pool status |
| POST | /pool/warm | Warm pool |

## Verification

### Cloud deployment test
```bash
# On a Linux server with KVM:
curl -sSL https://get.mvm.dev | bash
# Edit /etc/mvm/config: set API key
systemctl start mvm-daemon

# From laptop:
export MVM_REMOTE=https://server:19876
export MVM_API_KEY=the-key
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
import { Sandbox } from '@mvm/sdk';
const client = new Sandbox({ remote: 'https://server:19876', apiKey: 'key' });
const vm = await client.create('sdk-test');
const r = await vm.exec('echo works');
assert(r.output.trim() === 'works');
await vm.delete();
```
