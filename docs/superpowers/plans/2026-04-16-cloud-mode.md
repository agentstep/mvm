# Cloud Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make mvm work as both a local developer tool and a self-hosted cloud sandbox service with the same API, plus ship SDKs in Python, TypeScript, and Go.

**Architecture:** Two `http.Server` instances share one `ServeMux` — Unix socket serves `mux` directly (no auth), TCP serves `authMiddleware(mux)`. CLI gets `--remote`/`MVM_REMOTE` support. SDKs are thin HTTP clients (~400 lines each).

**Tech Stack:** Go stdlib (crypto/tls, net/http), Python httpx, TypeScript fetch

---

### Task 1: Configurable paths

Replace hardcoded path constants with env-configurable functions. This unblocks all cloud work since cloud servers use different paths.

**Files:**
- Modify: `internal/firecracker/config.go`
- Modify: `internal/firecracker/pool.go`
- Modify: `internal/server/routes.go`
- Modify: `internal/server/server.go`

- [ ] **Step 1: Replace constants with functions in config.go**

Replace lines 10-16:
```go
// Before:
const (
    CacheDir    = "/opt/mvm/cache"
    VMsDir      = "/opt/mvm/vms"
    KeyDir      = "/opt/mvm/keys"
    RunDir      = "/run/mvm"
    SnapshotDir = "/opt/mvm/snapshot"
)

// After:
func DataDir() string {
    if d := os.Getenv("MVM_DATA_DIR"); d != "" {
        return d
    }
    return "/opt/mvm"
}

func CacheDir() string    { return filepath.Join(DataDir(), "cache") }
func VMsDir() string      { return filepath.Join(DataDir(), "vms") }
func KeyDir() string      { return filepath.Join(DataDir(), "keys") }
func SnapshotDir() string { return filepath.Join(DataDir(), "snapshot") }

func RunDir() string {
    if d := os.Getenv("MVM_RUN_DIR"); d != "" {
        return d
    }
    return "/run/mvm"
}
```

Update all functions that reference these constants: `SocketPath`, `VsockUDSPath`, `VMDir`, `StartScript`, `StartScriptWithImage`, `StartExistingScript`, `StartFromSnapshotScript`, `GenerateConfig`.

- [ ] **Step 2: Update pool.go constants**

```go
// Before:
const (
    PoolDir         = "/opt/mvm/pool"
    poolSnapshotDir = "/opt/mvm/pool/snapshot"
)

// After:
func PoolDir() string         { return filepath.Join(DataDir(), "pool") }
func poolSnapshotDir() string { return filepath.Join(PoolDir(), "snapshot") }
```

Update `poolSlotDir`, `poolPidFile`, `poolReadyFile`, `poolSocketPath` and all callers.

- [ ] **Step 3: Update `snapshotsBaseDir` in routes.go**

```go
// Before:
const snapshotsBaseDir = "/opt/mvm/snapshots"

// After (use a function):
func snapshotsBaseDir() string { return filepath.Join(firecracker.DataDir(), "snapshots") }
```

Update all 6 references in the snapshot handlers.

- [ ] **Step 4: Rename `IsInsideLima()` to `IsLinux()` in server.go**

Update all callers (serve.go line 61).

- [ ] **Step 5: Build, test, vet**

```bash
go build ./... && TMPDIR=/tmp go test -race -count=1 ./internal/... && go vet ./internal/...
```

- [ ] **Step 6: Commit**

```bash
git commit -m "Make all paths configurable via MVM_DATA_DIR and MVM_RUN_DIR"
```

---

### Task 2: Dual TCP + Unix listeners with TLS

**Files:**
- Modify: `internal/server/server.go`

- [ ] **Step 1: Expand Config struct**

```go
type Config struct {
    SocketPath string
    PIDPath    string
    Store      *state.Store
    Executor   firecracker.Executor
    ListenAddr string // TCP address, e.g. "0.0.0.0:19876"
    TLSCert    string // Path to TLS certificate file
    TLSKey     string // Path to TLS private key file
    APIKey     string // Required for TCP connections
}
```

- [ ] **Step 2: Add dual-listener support to New() and Start()**

In `New()`, create the mux once. Create two `http.Server` instances:
- `unixServer` with `Handler: mux` (no auth)
- `tcpServer` with `Handler: authMiddleware(cfg.APIKey, mux)` (with auth) — only if `cfg.ListenAddr` is set

In `Start()`, use `errgroup.Group` to run both servers. Either failing causes both to shut down.

```go
func (s *Server) Start(ctx context.Context) error {
    s.WritePID()

    g, gctx := errgroup.WithContext(ctx)

    // Unix socket server (always)
    g.Go(func() error {
        log.Printf("Listening on %s", s.sockPath)
        err := s.unixServer.Serve(s.unixListener)
        if err == http.ErrServerClosed { return nil }
        return err
    })

    // TCP server (if configured)
    if s.tcpListener != nil {
        g.Go(func() error {
            log.Printf("Listening on %s (TCP+TLS)", s.cfg.ListenAddr)
            err := s.tcpServer.ServeTLS(s.tcpListener, s.cfg.TLSCert, s.cfg.TLSKey)
            if err == http.ErrServerClosed { return nil }
            return err
        })
    }

    // Shutdown on context cancel
    g.Go(func() error {
        <-gctx.Done()
        shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        s.unixServer.Shutdown(shutCtx)
        if s.tcpServer != nil { s.tcpServer.Shutdown(shutCtx) }
        return nil
    })

    return g.Wait()
}
```

Add TCP server timeouts:
```go
s.tcpServer = &http.Server{
    Handler:      authMiddleware(cfg.APIKey, mux),
    ReadTimeout:  30 * time.Second,
    WriteTimeout: 5 * time.Minute,
    IdleTimeout:  120 * time.Second,
}
```

Support `MVM_INSECURE=true` to skip TLS (for dev behind reverse proxy):
```go
if os.Getenv("MVM_INSECURE") == "true" {
    err = s.tcpServer.Serve(s.tcpListener)
} else {
    err = s.tcpServer.ServeTLS(s.tcpListener, s.cfg.TLSCert, s.cfg.TLSKey)
}
```

- [ ] **Step 3: Add `golang.org/x/sync/errgroup` dependency**

```bash
go get golang.org/x/sync
```

- [ ] **Step 4: Build and test**

- [ ] **Step 5: Commit**

```bash
git commit -m "Add dual TCP+Unix listeners with TLS support"
```

---

### Task 3: Auth middleware

**Files:**
- Create: `internal/server/auth.go`
- Create: `internal/server/auth_test.go`

- [ ] **Step 1: Create auth.go**

```go
package server

import (
    "net/http"
    "os"
    "strings"
)

// authMiddleware returns a handler that requires a valid API key on every
// request except GET /health (for load balancer probes).
func authMiddleware(apiKey string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Health check is always unauthenticated
        if r.Method == "GET" && r.URL.Path == "/health" {
            next.ServeHTTP(w, r)
            return
        }

        auth := r.Header.Get("Authorization")
        if !strings.HasPrefix(auth, "Bearer ") {
            http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
            return
        }
        token := strings.TrimPrefix(auth, "Bearer ")
        if token != apiKey {
            http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
            return
        }
        next.ServeHTTP(w, r)
    })
}

// LoadAPIKey reads the API key from flag, env var, or file (in that order).
func LoadAPIKey(flagValue, filePath string) string {
    if flagValue != "" {
        return flagValue
    }
    if key := os.Getenv("MVM_API_KEY"); key != "" {
        return key
    }
    if filePath != "" {
        if data, err := os.ReadFile(filePath); err == nil {
            return strings.TrimSpace(string(data))
        }
    }
    // Default file location
    if data, err := os.ReadFile("/etc/mvm/api-key"); err == nil {
        return strings.TrimSpace(string(data))
    }
    return ""
}
```

- [ ] **Step 2: Write auth tests**

```go
func TestAuthMiddleware_NoHeader(t *testing.T)      // expect 401
func TestAuthMiddleware_WrongKey(t *testing.T)       // expect 401
func TestAuthMiddleware_CorrectKey(t *testing.T)     // expect 200
func TestAuthMiddleware_HealthSkipsAuth(t *testing.T) // expect 200 without key
func TestLoadAPIKey_Priority(t *testing.T)           // flag > env > file
```

- [ ] **Step 3: Run tests**

```bash
TMPDIR=/tmp go test -race -count=1 ./internal/server/...
```

- [ ] **Step 4: Commit**

```bash
git commit -m "Add API key auth middleware with file/env/flag loading"
```

---

### Task 4: CLI remote mode

**Files:**
- Modify: `internal/server/client.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Add remote URL and API key support to Client**

Expand `NewClient` to accept options:

```go
type ClientOption func(*Client)

func WithRemote(url string) ClientOption {
    return func(c *Client) { c.remoteURL = url }
}

func WithAPIKey(key string) ClientOption {
    return func(c *Client) { c.apiKey = key }
}

func WithCACert(path string) ClientOption {
    return func(c *Client) { c.caCertPath = path }
}
```

When `remoteURL` is set, the client uses a standard `http.Client` with TLS instead of Unix socket dial. All requests go to `remoteURL` instead of `http://mvm/...`. Add `Authorization: Bearer` header if `apiKey` is set.

- [ ] **Step 2: Update DefaultClient() to check MVM_REMOTE**

```go
func DefaultClient() *Client {
    if remote := os.Getenv("MVM_REMOTE"); remote != "" {
        opts := []ClientOption{WithRemote(remote)}
        if key := os.Getenv("MVM_API_KEY"); key != "" {
            opts = append(opts, WithAPIKey(key))
        }
        if ca := os.Getenv("MVM_CA_CERT"); ca != "" {
            opts = append(opts, WithCACert(ca))
        }
        return NewClientWithOptions(opts...)
    }
    return NewClient(DefaultSocketPath())
}
```

- [ ] **Step 3: Update ExecInteractive to support TCP+TLS**

Replace hardcoded `net.DialTimeout("unix", c.socketPath, ...)` with a dialer that uses either Unix or TCP+TLS based on client config:

```go
func (c *Client) dial() (net.Conn, error) {
    if c.remoteURL != "" {
        // Parse host:port from URL, dial with TLS
        host := strings.TrimPrefix(c.remoteURL, "https://")
        tlsConf := c.tlsConfig()
        return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", host, tlsConf)
    }
    return net.DialTimeout("unix", c.socketPath, 5*time.Second)
}
```

Update `ExecInteractive` to use `c.dial()` instead of `net.DialTimeout("unix", ...)`.

- [ ] **Step 4: Add `--remote` and `--api-key` persistent flags to root.go**

```go
var (
    remoteURL string
    apiKey    string
)

rootCmd.PersistentFlags().StringVar(&remoteURL, "remote", "", "remote daemon URL (e.g. https://server:19876)")
rootCmd.PersistentFlags().StringVar(&apiKey, "api-key", "", "API key for remote daemon")
```

Set env vars from flags so `DefaultClient()` picks them up:
```go
if remoteURL != "" { os.Setenv("MVM_REMOTE", remoteURL) }
if apiKey != "" { os.Setenv("MVM_API_KEY", apiKey) }
```

- [ ] **Step 5: Build and test**

- [ ] **Step 6: Commit**

```bash
git commit -m "Add CLI remote mode with --remote and --api-key flags"
```

---

### Task 5: `serve start` flags

**Files:**
- Modify: `internal/cli/serve.go`

- [ ] **Step 1: Add new flags to `newServeStartCmd`**

```go
var (
    socketPath string
    listenAddr string
    tlsCert    string
    tlsKey     string
    apiKeyFlag string
    apiKeyFile string
)

cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path")
cmd.Flags().StringVar(&listenAddr, "listen", "", "TCP listen address (e.g. 0.0.0.0:19876)")
cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "TLS certificate file")
cmd.Flags().StringVar(&tlsKey, "tls-key", "", "TLS private key file")
cmd.Flags().StringVar(&apiKeyFlag, "api-key", "", "API key for TCP connections")
cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "File containing API key")
```

- [ ] **Step 2: Pass new config to server.New()**

```go
cfg := server.Config{
    SocketPath: socketPath,
    Store:      store,
    Executor:   executor,
    ListenAddr: listenAddr,
    TLSCert:    tlsCert,
    TLSKey:     tlsKey,
    APIKey:     server.LoadAPIKey(apiKeyFlag, apiKeyFile),
}
```

- [ ] **Step 3: Build and test**

- [ ] **Step 4: Commit**

```bash
git commit -m "Add --listen, --tls-cert, --tls-key, --api-key flags to serve start"
```

---

### Task 6: Systemd unit + install script

**Files:**
- Create: `deploy/mvm-daemon.service`
- Create: `scripts/install-cloud.sh`

- [ ] **Step 1: Create systemd unit**

```ini
[Unit]
Description=mvm sandbox daemon
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/mvm serve start --listen 0.0.0.0:19876 --tls-cert /etc/mvm/cert.pem --tls-key /etc/mvm/key.pem --api-key-file /etc/mvm/api-key
Environment=MVM_DATA_DIR=/var/mvm
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 2: Create install script**

`scripts/install-cloud.sh` — downloads mvm + Firecracker, creates dirs, generates self-signed TLS cert, generates random API key, installs systemd unit, starts service. Checks `/dev/kvm` first.

- [ ] **Step 3: Commit**

```bash
git commit -m "Add systemd unit and cloud install script"
```

---

### Task 7: Go SDK

**Files:**
- Create: `sdk/client.go`
- Create: `sdk/types.go`
- Create: `sdk/client_test.go`

- [ ] **Step 1: Create types.go with all public request/response types**

Duplicate types from `internal/server/routes.go` — SDK must not import internal packages. Include: `CreateVMRequest`, `VMResponse`, `ExecRequest`, `ExecResponse`, `SnapshotInfo`, `BuildStep`, `ImageInfo`, `PoolStatus`, `PortMap`.

- [ ] **Step 2: Create client.go**

Public `Client` with methods matching `internal/server/client.go` but using the SDK's own types. Constructor: `New(url string, opts ...Option)`.

Options: `WithAPIKey(key)`, `WithCACert(path)`, `WithUnixSocket(path)`.

Methods: `CreateVM`, `DeleteVM`, `ListVMs`, `Exec`, `ExecStream`, `StopVM`, `PauseVM`, `ResumeVM`, `SnapshotCreate`, `SnapshotRestore`, `SnapshotList`, `SnapshotDelete`, `Build`, `ImageList`, `ImageDelete`, `PoolStatus`, `PoolWarm`, `Health`.

Exclude `ExecInteractive` (raw PTY relay is not a typical SDK use case).

- [ ] **Step 3: Write tests using httptest.Server**

- [ ] **Step 4: Build and test**

```bash
go test -race -count=1 ./sdk/...
```

- [ ] **Step 5: Commit**

```bash
git commit -m "Add public Go SDK at github.com/agentstep/mvm/sdk"
```

---

### Task 8: Python SDK

**Files:**
- Create: `sdks/python/mvm_sandbox/__init__.py`
- Create: `sdks/python/mvm_sandbox/client.py`
- Create: `sdks/python/mvm_sandbox/types.py`
- Create: `sdks/python/mvm_sandbox/exceptions.py`
- Create: `sdks/python/pyproject.toml`
- Create: `sdks/python/tests/test_client.py`

- [ ] **Step 1: Create pyproject.toml**

```toml
[project]
name = "mvm-sandbox"
version = "0.1.0"
description = "Python SDK for mvm sandbox"
requires-python = ">=3.9"
dependencies = ["httpx>=0.27"]

[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"
```

- [ ] **Step 2: Create types.py**

Dataclasses: `VM`, `ExecResult`, `Snapshot`, `Image`, `PoolStatus`, `PortMap`, `BuildStep`.

- [ ] **Step 3: Create exceptions.py**

`MVMError`, `AuthError`, `NotFoundError`, `ConflictError`, `ServerError`.

- [ ] **Step 4: Create client.py**

`Sandbox` class with `connect()` classmethod. `VM` class returned by `create()` with `exec()`, `stop()`, `delete()`, `snapshot()`, `restore()` methods. Uses `httpx.Client` with auth header injection and custom CA cert support.

- [ ] **Step 5: Write tests with httpx mock transport**

- [ ] **Step 6: Run tests**

```bash
cd sdks/python && pip install -e ".[dev]" && pytest tests/ -v
```

- [ ] **Step 7: Commit**

```bash
git commit -m "Add Python SDK (mvm-sandbox) with httpx client"
```

---

### Task 9: TypeScript SDK

**Files:**
- Create: `sdks/typescript/src/index.ts`
- Create: `sdks/typescript/src/client.ts`
- Create: `sdks/typescript/src/types.ts`
- Create: `sdks/typescript/src/errors.ts`
- Create: `sdks/typescript/package.json`
- Create: `sdks/typescript/tsconfig.json`
- Create: `sdks/typescript/tests/client.test.ts`

- [ ] **Step 1: Create package.json**

```json
{
  "name": "@agentstep/mvm-sdk",
  "version": "0.1.0",
  "description": "TypeScript SDK for mvm sandbox",
  "main": "dist/index.js",
  "types": "dist/index.d.ts",
  "scripts": {
    "build": "tsc",
    "test": "vitest run"
  }
}
```

- [ ] **Step 2: Create types.ts**

TypeScript interfaces: `VM`, `ExecResult`, `Snapshot`, `Image`, `PoolStatus`, `CreateVMOptions`, `SandboxOptions`.

- [ ] **Step 3: Create errors.ts**

`MVMError`, `AuthError`, `NotFoundError`, `ConflictError`.

- [ ] **Step 4: Create client.ts**

`Sandbox` class with constructor taking `SandboxOptions`. `VM` class with `exec()`, `stop()`, `delete()`, `snapshot()`, `restore()`. Uses `fetch()` with auth headers. Custom CA via Node.js `https.Agent`.

- [ ] **Step 5: Write tests with MSW (Mock Service Worker)**

- [ ] **Step 6: Build and test**

```bash
cd sdks/typescript && npm install && npm run build && npm test
```

- [ ] **Step 7: Commit**

```bash
git commit -m "Add TypeScript SDK (@agentstep/mvm-sdk) with fetch client"
```

---

## Verification

### End-to-end cloud test
```bash
# Build everything
go build -o bin/mvm ./cmd/mvm/
GOOS=linux GOARCH=arm64 go build -o bin/mvm-linux ./cmd/mvm/

# Local test (existing functionality preserved)
mvm pool status
mvm start test1 && mvm exec test1 -- echo "local works" && mvm delete test1 --force

# Cloud test (on a Linux server with KVM)
scp bin/mvm-linux server:/usr/local/bin/mvm
ssh server 'mvm serve start --listen 0.0.0.0:19876 --api-key test-key' &
MVM_REMOTE=http://server:19876 MVM_API_KEY=test-key MVM_INSECURE=true mvm pool status

# SDK tests
cd sdk && go test -race ./...
cd sdks/python && pytest tests/ -v
cd sdks/typescript && npm test
```
