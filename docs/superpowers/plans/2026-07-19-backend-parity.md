# mvm Backend Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close three cross-backend feature gaps between mvm's two VM backends — Firecracker (VMs managed by a daemon reached via `internal/server.Client`; the CLI is thin) and applevz (VMs driven directly by the CLI process via Apple's Virtualization.framework; state lives in the local `state.Store`, no daemon) — so a documented capability behaves the same regardless of which backend a host is configured with:

1. `--startup`/`--secret` currently error out on the daemon/Firecracker path (`internal/cli/start.go:124,149`) but work on applevz.
2. `--image` (custom rootfs) currently has no effect on applevz — `runStartAppleVZ` doesn't even accept an image parameter — but works on the daemon/Firecracker path.
3. `mvm logs --boot` reads Firecracker's console log by shelling into Lima (`internal/cli/logs.go`'s `showBootLog`) instead of going through the daemon, unlike every other VM-lifecycle command (`list`/`inspect`/`delete`/`exec` all route through `internal/server.Client`).

**Architecture:** No change to the two-backend shape. Each phase adds one small, focused seam:

- **Phase 1** adds a `Secrets []string` (names only, never values) field to the daemon's `CreateVMRequest`/`state.VM`, and a `recipeAgent` interface so `internal/cli/startup.go`'s existing recipe runner can drive either backend's exec transport (`*agentclient.Client` over vsock for applevz, `*server.Client` over HTTP for the daemon) without duplicating the recipe logic.
- **Phase 2** adds a daemon image-download endpoint. This is required, not optional: reading `firecracker.DataDir()` (`internal/firecracker/config.go:12-18`, defaults to `/opt/mvm`, only overridden to `/var/mvm` for cloud installs — `scripts/install-cloud.sh:383`) against Lima's mount configuration (`.mounts=[{"location":"~","writable":true}]`, `internal/lima/lima.go:168` — only the user's home directory is virtiofs-shared with macOS) proves that a `mvm build`-produced image at `firecracker.CacheDir()/<tag>.ext4` lives **inside the daemon's own Linux filesystem** (Lima's guest root, or a remote cloud box's `/var/mvm`), never on the macOS host. applevz boots directly on macOS from `~/.mvm/cache/<name>.ext4` with no daemon in the loop at all. Without an explicit fetch step, a built image is permanently stuck wherever it was built and applevz can never see it.
- **Phase 3** adds a matching `GET /vms/{name}/logs` daemon endpoint (NDJSON streaming, following `handleExec`'s `Stream:true` pattern) for boot/console logs specifically, since the daemon process runs natively on the same Linux host as the Firecracker console log file (`firecracker.VMDir(name)/firecracker.log`) and can read it directly with `os.Open` — no shell-out needed. (Guest journal logs, the non-`--boot` path, already route through the daemon today via the existing exec endpoint — see the Phase 3 intro below for why that part of the file needed no change.)

**Tech Stack:** Go 1.22+, cobra, stdlib `net/http` + `encoding/json` for the daemon API, stdlib-only tests (no testify) — matches `internal/cli`'s and `internal/server`'s existing conventions.

## Global Constraints

- **Security invariant (Phase 1, load-bearing for the whole plan): secret VALUES never transit to or rest in the daemon — only secret NAMES do.** A secret's plaintext value only ever exists in the CLI process's memory (decrypted on demand from `internal/secrets.Store`, which is itself AES-256-GCM-sealed at rest under `~/.mvm/`) and in the exec script text sent to a guest at call time — identical exposure to what `runExecAppleVZ` already does today over vsock. The daemon's `CreateVMRequest.Secrets` field, `state.VM.Secrets`, and `state.VMSpec.Secrets` all hold names only; nothing in this plan adds a code path where a decrypted value is marshaled into a daemon request or response body. Every test in Phase 1 that touches the wire format asserts this directly (see Task 1).
- The three phases are independently shippable in the order given (1, 2, 3) — no phase's code depends on another phase's changes landing first, though Phase 1 and Phase 2 both touch `internal/cli/start.go`'s `runStartAppleVZ`/`runStart`, so implementing them out of order will produce a merge conflict, not a logic conflict.
- Match existing code style: tabs, stdlib-only tests (no testify), matching every other file in `internal/cli` and `internal/server`.
- Repo module path is `github.com/agentstep/mvm`. Run all commands from `/Users/paulmeller/Projects/firecracker`.
- Hardware/daemon-dependent smoke-test steps (anything requiring a real Firecracker VM, a real Apple VZ boot, or a live daemon) are best-effort, same convention as `docs/superpowers/plans/2026-07-19-mvm-run.md` Task 4 Step 5: run them if the environment supports it, otherwise skip and note it in the report. They are never a blocker for the unit-tested portions of a task.
- No behavior change to any command not named in this plan.

---

## Phase 1: `--startup`/`--secret` on the daemon/Firecracker path

Today, `internal/cli/start.go`'s `runStart` hard-rejects `--startup`/`--secret` for every non-applevz backend at two call sites:

```go
// line ~122-127 (MVM_REMOTE / cloud mode)
if os.Getenv("MVM_REMOTE") != "" {
    if startup != nil || len(secretNames) > 0 {
        return fmt.Errorf("--startup/--secret are not yet supported on the daemon/firecracker path")
    }
    return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image)
}
...
// line ~148-150 (local daemon mode)
if startup != nil || len(secretNames) > 0 {
    return fmt.Errorf("--startup/--secret are not yet supported on the daemon/firecracker path")
}
return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image)
```

`validateSecretsExist(secretNames)` (in `internal/cli/secret.go`) already runs unconditionally before either branch, so a typo'd secret name is already caught for both backends today — only the actual attach/inject/run-recipe behavior is missing on the daemon path.

### Task 1: Thread secret NAMES through `CreateVMRequest` → `state.VM`

**Files:**
- Modify: `internal/server/routes.go`
- Modify: `internal/server/routes_test.go`

**Interfaces:**
- Produces: `CreateVMRequest.Secrets []string` (new field), wired into `specFromCreateRequest` and `handleCreateVM`. Task 3 (`daemonSecretEnv`) and Task 4 (`runStartViaDaemon`) consume this.

- [ ] **Step 1: Write the failing test**

Add `"strings"` to `internal/server/routes_test.go`'s import block (it currently imports `bytes`, `encoding/json`, `net/http`, `net/http/httptest`, `os`, `path/filepath`, `testing`, `time`, and `github.com/agentstep/mvm/internal/state`):

```go
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)
```

Append to `internal/server/routes_test.go`:

```go
func TestHandleCreateVMPersistsSecretNamesOnly(t *testing.T) {
	s, store := testServer(t)

	body, _ := json.Marshal(CreateVMRequest{
		Name:    "web",
		Secrets: []string{"OPENAI_API_KEY", "DB_PASSWORD"},
	})
	req := httptest.NewRequest("POST", "/vms", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}
	// The response body must never carry secret values — only names ever
	// reach the daemon in the first place (the security invariant this
	// whole phase exists to uphold), but assert the wire shape directly too:
	// a name showing up in a response is one accidental rename away from a
	// value showing up there.
	if strings.Contains(w.Body.String(), "OPENAI_API_KEY") {
		t.Errorf("response body echoes a secret name back over the wire: %s", w.Body.String())
	}

	vm, err := store.GetVM("web")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if len(vm.Secrets) != 2 || vm.Secrets[0] != "OPENAI_API_KEY" || vm.Secrets[1] != "DB_PASSWORD" {
		t.Errorf("vm.Secrets = %v, want [OPENAI_API_KEY DB_PASSWORD] persisted from the request", vm.Secrets)
	}
	if vm.Spec == nil || len(vm.Spec.Secrets) != 2 {
		t.Errorf("vm.Spec.Secrets = %v, want the same names surfaced via inspect", vm.Spec.Secrets)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestHandleCreateVMPersistsSecretNamesOnly -v`
Expected: FAIL — compile error `unknown field Secrets in struct literal of type CreateVMRequest`.

- [ ] **Step 3: Write minimal implementation**

In `internal/server/routes.go`, add a field to `CreateVMRequest` (after `Image`):

```go
type CreateVMRequest struct {
	Name      string          `json:"name"`
	Cpus      int             `json:"cpus,omitempty"`
	MemoryMB  int             `json:"memory_mb,omitempty"`
	Ports     []state.PortMap `json:"ports,omitempty"`
	NetPolicy string          `json:"net_policy,omitempty"`
	Volumes   []string        `json:"volumes,omitempty"`
	Seccomp   string          `json:"seccomp,omitempty"`
	Image     string          `json:"image,omitempty"`
	// Secrets holds attached secret NAMES ONLY — never values. See the
	// package-level security invariant in this plan's Global Constraints.
	Secrets []string `json:"secrets,omitempty"`
}
```

Update `specFromCreateRequest`:

```go
func specFromCreateRequest(req CreateVMRequest) *state.VMSpec {
	return &state.VMSpec{
		Image:     req.Image,
		Cpus:      req.Cpus,
		MemoryMB:  req.MemoryMB,
		Ports:     req.Ports,
		Volumes:   req.Volumes,
		NetPolicy: req.NetPolicy,
		Seccomp:   req.Seccomp,
		Secrets:   req.Secrets,
	}
}
```

Update `handleCreateVM`'s initial `vm := &state.VM{...}` (the `ReserveVM` call, not the later `UpdateVM`):

```go
	now := time.Now()
	vm := &state.VM{
		Name:      req.Name,
		Status:    "starting",
		Ports:     req.Ports,
		NetPolicy: req.NetPolicy,
		Cpus:      req.Cpus,
		MemoryMB:  req.MemoryMB,
		Secrets:   req.Secrets,
		CreatedAt: now,
		Spec:      specFromCreateRequest(req),
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run TestHandleCreateVM -v`
Expected: PASS, including the new test and all pre-existing `TestHandleCreateVM*` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/server/routes.go internal/server/routes_test.go
git commit -m "feat(server): thread attached-secret NAMES through CreateVMRequest to state.VM"
```

---

### Task 2: `daemonSecretEnv` — per-exec secret injection on the daemon path

Mirrors what `runExecAppleVZ` already does (`internal/cli/exec.go:126-134`): decrypt attached secrets from host memory and append them as `KEY=VALUE` env exports before building the exec script. The daemon path needs to first learn *which* secrets are attached. Two sources exist:

- **Local mode** (no `MVM_REMOTE`): `~/.mvm/state.json` is virtiofs-shared between macOS and the Lima guest the daemon runs in (`server.DefaultStatePath`'s doc comment: "Same path on macOS and inside Lima (shared via writable virtiofs mount)"). After Task 1, the daemon's own write of `vm.Secrets` at create time is therefore already visible to the CLI's local `store` — no network round trip needed.
- **Cloud/remote mode** (`MVM_REMOTE` set): the daemon runs on a genuinely separate machine with no shared filesystem at all; local `store` has no entry for that VM. Fall back to `sc.InspectVM`, which already returns `Spec.Secrets` (names only) via the existing `/v1/vms/{name}` endpoint.

**Files:**
- Modify: `internal/cli/exec.go`
- Modify: `internal/cli/exec_test.go`

**Interfaces:**
- Consumes: `state.Store.GetVM`, `server.Client.InspectVM` (both existing), `secretEnvVars` (`internal/cli/secret.go`, existing).
- Produces: `func daemonSecretEnv(ctx context.Context, store *state.Store, sc *server.Client, name string) ([]string, error)`. `runExec` calls it exactly as declared here.

- [ ] **Step 1: Write the failing test**

Update `internal/cli/exec_test.go`'s import block to:

```go
package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)
```

Append:

```go
// === daemonSecretEnv ===

func TestDaemonSecretEnvUsesLocalStoreWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "state.json"))
	store.AddVM(&state.VM{Name: "web", Backend: "firecracker", Secrets: []string{"MISSING_SECRET"}, CreatedAt: time.Now()})

	// Point sc at a socket that doesn't exist — if this test passes,
	// daemonSecretEnv resolved secrets from the local store and never
	// dialed sc at all.
	sc := server.NewClient(filepath.Join(dir, "no-such.sock"))

	_, err := daemonSecretEnv(context.Background(), store, sc, "web")
	// secretEnvVars fails because MISSING_SECRET was never `mvm secret put`
	// in this test — the ONLY way this can fail here is via the local-store
	// path; an InspectVM round trip against a nonexistent socket would fail
	// with a dial/connection error instead, not "not found".
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a secret-not-found error from the local-store path", err)
	}
}

func TestDaemonSecretEnvUsesLocalStoreEvenWithNoSecrets(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "state.json"))
	store.AddVM(&state.VM{Name: "web", Backend: "firecracker", CreatedAt: time.Now()})
	sc := server.NewClient(filepath.Join(dir, "no-such.sock")) // unreachable — proves no round trip happened

	env, err := daemonSecretEnv(context.Background(), store, sc, "web")
	if err != nil {
		t.Fatalf("daemonSecretEnv: %v", err)
	}
	if env != nil {
		t.Errorf("env = %v, want nil (no secrets attached, no daemon round trip needed)", env)
	}
}

func TestDaemonSecretEnvFallsBackToInspectVM(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{
			VMResponse: server.VMResponse{Name: "web"},
			Spec:       &state.VMSpec{Secrets: []string{"MISSING_SECRET"}},
		})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	sc := server.NewRemoteClient(ts.URL, "", "")

	// Empty store — this VM is unknown locally, the way a cloud/remote VM
	// always is (no shared filesystem with a genuinely remote daemon host).
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	_, err := daemonSecretEnv(context.Background(), store, sc, "web")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want the InspectVM fallback to still surface a secret-not-found error", err)
	}
}

func TestDaemonSecretEnvNoSecretsViaInspectVM(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{VMResponse: server.VMResponse{Name: "web"}})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	sc := server.NewRemoteClient(ts.URL, "", "")
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	env, err := daemonSecretEnv(context.Background(), store, sc, "web")
	if err != nil {
		t.Fatalf("daemonSecretEnv: %v", err)
	}
	if env != nil {
		t.Errorf("env = %v, want nil for a VM with no secrets attached", env)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestDaemonSecretEnv -v`
Expected: FAIL — compile error `undefined: daemonSecretEnv`.

- [ ] **Step 3: Write minimal implementation**

Update `internal/cli/exec.go`'s import block to add `"github.com/agentstep/mvm/internal/server"`:

```go
import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)
```

Add above `runExec`:

```go
// daemonSecretEnv resolves the VM's attached secret NAMES and decrypts them
// from host memory, returning KEY=VALUE entries for buildExecScript — the
// daemon-path equivalent of the inline block in runExecAppleVZ below. Local
// mode resolves for free from the shared state store (see the Task 2 doc
// comment in the backend-parity plan); cloud/remote mode falls back to
// asking the daemon for the names via InspectVM (never values).
func daemonSecretEnv(ctx context.Context, store *state.Store, sc *server.Client, name string) ([]string, error) {
	if vm, err := store.GetVM(name); err == nil && vm != nil {
		return secretEnvVars(vm.Secrets)
	}
	info, err := sc.InspectVM(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("look up secrets for %q: %w", name, err)
	}
	if info.Spec == nil || len(info.Spec.Secrets) == 0 {
		return nil, nil
	}
	return secretEnvVars(info.Spec.Secrets)
}
```

Update `runExec` (only the daemon branch changes):

```go
func runExec(store *state.Store, name string, remoteArgs []string, interactive bool, workdir string, envVars []string, user string) error {
	// Apple VZ VMs aren't managed by the daemon — exec directly against the
	// per-VM mvm-vz helper's vsock-bridged agent.
	if vm, _ := store.GetVM(name); vm != nil && vm.Backend == "applevz" {
		return runExecAppleVZ(store, vm, remoteArgs, interactive, workdir, envVars, user)
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	// Inject the VM's attached secrets, decrypted from host memory at call
	// time — mirrors runExecAppleVZ above. Only secret NAMES ever left this
	// process to reach the daemon (at `mvm start --secret`); values are
	// decrypted here and never travel further than the exec script sent to
	// the daemon.
	secretEnv, err := daemonSecretEnv(context.Background(), store, sc, name)
	if err != nil {
		return err
	}
	envVars = append(envVars, secretEnv...)

	script := buildExecScript(remoteArgs, workdir, envVars, user)

	if interactive {
		...
```

(The rest of `runExec` is unchanged — only the new block between `requireDaemon()` and `buildExecScript(...)` is inserted.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestDaemonSecretEnv -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/exec.go internal/cli/exec_test.go
git commit -m "feat(cli): inject attached secrets per-exec on the daemon/Firecracker path"
```

---

### Task 3: `recipeAgent` interface — decouple the startup recipe runner from applevz

**Files:**
- Modify: `internal/cli/startup.go`
- Create: `internal/cli/startup_test.go`
- Modify: `internal/cli/start.go` (one call-site update, applevz side only — see Task 4 for the daemon call site)

**Interfaces:**
- Produces: `type recipeAgent interface { Exec(ctx context.Context, command, stdin string) (output string, exitCode int, err error) }`, `type applevzRecipeAgent struct{ c *agentclient.Client }`, `type daemonRecipeAgent struct{ sc *server.Client; vmName string }` (both satisfy `recipeAgent`). `runStartupRecipe`'s signature changes from `agent *agentclient.Client` to `agent recipeAgent`. Task 4's `runStartViaDaemon` consumes `daemonRecipeAgent`.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/startup_test.go`:

```go
package cli

import (
	"context"
	"strings"
	"testing"
)

// fakeRecipeAgent implements recipeAgent for tests — no vsock, no daemon,
// no real VM.
type fakeRecipeAgent struct {
	calls  []string
	execFn func(command string) (string, int, error)
}

func (f *fakeRecipeAgent) Exec(ctx context.Context, command, stdin string) (string, int, error) {
	f.calls = append(f.calls, command)
	if f.execFn != nil {
		return f.execFn(command)
	}
	return "", 0, nil
}

func TestRunStartupRecipeRunsCommandsInOrder(t *testing.T) {
	agent := &fakeRecipeAgent{}
	spec := &StartupSpec{
		Workdir: "/workspace",
		Commands: []StartupCommand{
			{Name: "install", Run: "npm install"},
			{Name: "build", Run: "npm run build"},
		},
	}
	if err := runStartupRecipe(context.Background(), agent, spec, newPhaseTimer(), func(string, ...any) {}); err != nil {
		t.Fatalf("runStartupRecipe: %v", err)
	}
	if len(agent.calls) != 3 { // mkdir workdir + 2 commands
		t.Fatalf("calls = %v, want 3 (mkdir + 2 commands)", agent.calls)
	}
	if !strings.Contains(agent.calls[1], "npm install") || !strings.Contains(agent.calls[2], "npm run build") {
		t.Errorf("calls = %v, want install then build in order", agent.calls)
	}
}

func TestRunStartupRecipeFailsFastOnNonZeroExit(t *testing.T) {
	agent := &fakeRecipeAgent{execFn: func(command string) (string, int, error) {
		if strings.Contains(command, "this-fails") {
			return "boom", 1, nil
		}
		return "", 0, nil
	}}
	spec := &StartupSpec{
		Commands: []StartupCommand{
			{Name: "bad", Run: "this-fails"},
			{Name: "unreached", Run: "echo never"},
		},
	}
	err := runStartupRecipe(context.Background(), agent, spec, newPhaseTimer(), func(string, ...any) {})
	if err == nil {
		t.Fatal("runStartupRecipe() = nil, want an error from the failing command")
	}
	if len(agent.calls) != 2 { // mkdir + the failing command; "unreached" must never run
		t.Errorf("calls = %v, want exactly 2 (mkdir + failing command)", agent.calls)
	}
}

func TestRecipeAgentAdaptersSatisfyInterface(t *testing.T) {
	var _ recipeAgent = applevzRecipeAgent{}
	var _ recipeAgent = daemonRecipeAgent{}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestRunStartupRecipe|TestRecipeAgentAdapters' -v`
Expected: FAIL — compile errors `undefined: recipeAgent`, `undefined: applevzRecipeAgent`, `undefined: daemonRecipeAgent`, and a type mismatch on `runStartupRecipe(ctx, agent, ...)` since `agent` is `*fakeRecipeAgent`, not `*agentclient.Client`.

- [ ] **Step 3: Write minimal implementation**

In `internal/cli/startup.go`, add `"github.com/agentstep/mvm/internal/server"` to the import block:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/server"
)
```

Add above `runStartupRecipe`:

```go
// recipeAgent is the minimal exec surface runStartupRecipe needs. Two
// backends satisfy it: applevzRecipeAgent wraps the vsock-based
// *agentclient.Client (Apple VZ, no daemon in the loop) and
// daemonRecipeAgent wraps *server.Client (Firecracker, via the daemon's
// /vms/{name}/exec endpoint) — see internal/cli/exec.go's runExec for the
// same backend split on the exec path. Depending on this interface instead
// of *agentclient.Client directly is what lets `mvm start --startup` work
// on both backends from one recipe runner.
type recipeAgent interface {
	Exec(ctx context.Context, command, stdin string) (output string, exitCode int, err error)
}

// applevzRecipeAgent adapts *agentclient.Client's (*ExecResult, error) shape
// to recipeAgent.
type applevzRecipeAgent struct{ c *agentclient.Client }

func (a applevzRecipeAgent) Exec(ctx context.Context, command, stdin string) (string, int, error) {
	res, err := a.c.Exec(ctx, command, stdin)
	if err != nil {
		return "", -1, err
	}
	return res.Output, res.ExitCode, nil
}

// daemonRecipeAgent adapts *server.Client's per-VM Exec to recipeAgent.
// server.Client.Exec has no stdin parameter — every runStartupRecipe call
// site below passes "" anyway, so the adapter just ignores it.
type daemonRecipeAgent struct {
	sc     *server.Client
	vmName string
}

func (d daemonRecipeAgent) Exec(ctx context.Context, command, _ string) (string, int, error) {
	return d.sc.Exec(ctx, d.vmName, command)
}
```

Change `runStartupRecipe`'s signature and every internal `agent.Exec` call site:

```go
func runStartupRecipe(ctx context.Context, agent recipeAgent, spec *StartupSpec, timer *phaseTimer, logf func(string, ...any)) error {
	envp := spec.envPrefix()
	wd := shellQuote(spec.Workdir)

	if _, _, err := agent.Exec(ctx, "mkdir -p "+wd, ""); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}

	if spec.Git != nil && spec.Git.URL != "" {
		logf("  Startup: cloning %s...\n", spec.Git.URL)
		branch := ""
		if spec.Git.Ref != "" {
			branch = "--branch " + shellQuote(spec.Git.Ref) + " "
		}
		clone := fmt.Sprintf("git clone --depth 1 %s%s %s", branch, shellQuote(spec.Git.URL), wd)
		if _, exitCode, err := agent.Exec(ctx, clone, ""); err != nil {
			return fmt.Errorf("git clone: %w", err)
		} else if exitCode != 0 {
			return fmt.Errorf("git clone failed (exit %d)", exitCode)
		}
		timer.mark("startup_git")
	}

	for _, c := range spec.Commands {
		label := c.Name
		if label == "" {
			label = "command"
		}
		full := fmt.Sprintf("cd %s; %s%s", wd, envp, c.Run)
		if c.Background {
			logf("  Startup: %s (background)...\n", label)
			full = fmt.Sprintf("cd %s; %ssetsid sh -c %s >/tmp/%s.log 2>&1 < /dev/null &",
				wd, envp, shellQuote(c.Run), shellQuote(label))
			if _, _, err := agent.Exec(ctx, full, ""); err != nil {
				return fmt.Errorf("startup %q: %w", label, err)
			}
		} else {
			logf("  Startup: %s...\n", label)
			output, exitCode, err := agent.Exec(ctx, full, "")
			if err != nil {
				return fmt.Errorf("startup %q: %w", label, err)
			}
			if exitCode != 0 {
				return fmt.Errorf("startup %q failed (exit %d): %s", label, exitCode, strings.TrimSpace(output))
			}
		}
		timer.mark("startup_" + label)
	}

	if spec.Ready != nil && spec.Ready.HTTP != "" {
		timeout := spec.Ready.TimeoutSeconds
		if timeout <= 0 {
			timeout = 30
		}
		logf("  Startup: waiting for %s (≤%ds)...\n", spec.Ready.HTTP, timeout)
		poll := fmt.Sprintf(
			"for i in $(seq 1 %d); do (wget -qO- %s >/dev/null 2>&1 || curl -fsS %s >/dev/null 2>&1) && exit 0; sleep 1; done; exit 1",
			timeout, shellQuote(spec.Ready.HTTP), shellQuote(spec.Ready.HTTP))
		rctx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
		defer cancel()
		_, exitCode, err := agent.Exec(rctx, poll, "")
		if err != nil {
			return fmt.Errorf("ready check: %w", err)
		}
		if exitCode != 0 {
			return fmt.Errorf("ready check timed out after %ds (%s never answered)", timeout, spec.Ready.HTTP)
		}
		timer.mark("startup_ready")
	}

	return nil
}
```

In `internal/cli/start.go`, update `runStartAppleVZ`'s call site (the only existing call to `runStartupRecipe`):

```go
	if startupErr == nil {
		startupErr = runStartupRecipe(context.Background(), applevzRecipeAgent{agent}, startup, timer, logf)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestRunStartupRecipe|TestRecipeAgentAdapters' -v && go build ./...`
Expected: PASS, and a clean build (this confirms the applevz call site compiles against the new signature).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/startup.go internal/cli/startup_test.go internal/cli/start.go
git commit -m "refactor(cli): decouple startup-recipe runner from applevz via a recipeAgent interface"
```

---

### Task 4: Wire the daemon path — drop the guards, run create + startup recipe

**Files:**
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/start_test.go`

**Interfaces:**
- Consumes: `server.CreateVMRequest.Secrets` (Task 1), `daemonSecretEnv`/pattern (Task 2 — not reused directly here since secrets are already known at this call site as `secretNames`), `recipeAgent`/`daemonRecipeAgent` (Task 3), `waitForReady` (`internal/cli/run.go`, existing), `secretEnvVars` (`internal/cli/secret.go`, existing).
- Produces: `runStartViaDaemon` gains two parameters: `func runStartViaDaemon(name string, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, cpus, memoryMB int, image string, startup *StartupSpec, secretNames []string) error`.

- [ ] **Step 1: Write the failing test**

Update `internal/cli/start_test.go`'s import block to:

```go
package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)
```

Append:

```go
// === runStartViaDaemon: secrets + the removed guards ===

func TestRunStartViaDaemonSendsSecretNamesNotValues(t *testing.T) {
	var captured server.CreateVMRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/vms":
			json.NewDecoder(r.Body).Decode(&captured)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(server.VMResponse{Name: captured.Name, Status: "running", GuestIP: "10.0.0.2"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)

	err := runStartViaDaemon("web", nil, "open", nil, "", 0, 0, "", nil, []string{"OPENAI_API_KEY"})
	if err != nil {
		t.Fatalf("runStartViaDaemon: %v", err)
	}
	if len(captured.Secrets) != 1 || captured.Secrets[0] != "OPENAI_API_KEY" {
		t.Errorf("captured.Secrets = %v, want [OPENAI_API_KEY]", captured.Secrets)
	}
}

func TestRunStartNoLongerRejectsStartupOrSecretsOnDaemonPath(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)

	// "OPENAI_API_KEY" isn't a real stored secret in this test's environment
	// (no MVM_SECRET_KEY, no secret store), so validateSecretsExist rejects
	// it — but critically, it must fail with a secret-not-found error, not
	// the old "not yet supported on the daemon/firecracker path" message.
	err := runStart(store, "web", true, nil, "open", nil, "", "", 0, 0, "", false, nil, []string{"OPENAI_API_KEY"})
	if err == nil || strings.Contains(err.Error(), "not yet supported") {
		t.Fatalf("runStart() = %v, want a secret-not-found error, not the old unsupported-path rejection", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want it to mention the missing secret", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestRunStartViaDaemon|TestRunStartNoLongerRejects' -v`
Expected: FAIL — compile error, `runStartViaDaemon` still takes 8 args, called here with 10.

- [ ] **Step 3: Write minimal implementation**

In `internal/cli/start.go`, remove both guard blocks in `runStart`:

```go
	// Cloud/remote mode: the local state doesn't matter — the daemon is
	// the source of truth. Skip the local init check entirely.
	if os.Getenv("MVM_REMOTE") != "" {
		return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image, startup, secretNames)
	}

	initialized, err := store.IsInitialized()
	if err != nil {
		return err
	}
	if !initialized {
		return fmt.Errorf("mvm is not initialized. Run: mvm init")
	}

	backend := store.GetBackend()

	// Apple VZ path — dispatch to separate function
	if backend == "applevz" {
		out := outHuman
		if jsonOut {
			out = outJSON
		}
		_, err := runStartAppleVZ(store, name, detach, ports, netPolicy, cpus, memoryMB, volumes, out, startup, secretNames)
		return err
	}

	// Firecracker path: route through daemon
	return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image, startup, secretNames)
}
```

(Both `if startup != nil || len(secretNames) > 0 { return fmt.Errorf(...) }` blocks are deleted outright — everything else in `runStart` is unchanged.)

Replace `runStartViaDaemon`:

```go
// runStartViaDaemon creates a VM by calling the daemon's /vms endpoint.
// Used for both local-mode (Unix socket) and cloud-mode (TCP+TLS).
func runStartViaDaemon(name string, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, cpus, memoryMB int, image string, startup *StartupSpec, secretNames []string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()
	resp, err := sc.CreateVM(ctx, server.CreateVMRequest{
		Name:      name,
		Cpus:      cpus,
		MemoryMB:  memoryMB,
		Ports:     ports,
		NetPolicy: netPolicy,
		Volumes:   volumes,
		Seccomp:   seccomp,
		Image:     image,
		// Only secret NAMES cross this boundary — see the package-level
		// security invariant in this plan's Global Constraints. Values are
		// decrypted client-side, per-exec, exactly like runExecAppleVZ does
		// for applevz (internal/cli/exec.go).
		Secrets: secretNames,
	})
	if err != nil {
		return err
	}

	fmt.Printf("\n  %s is running!\n", resp.Name)
	fmt.Printf("    IP:   %s\n", resp.GuestIP)
	for _, p := range resp.Ports {
		fmt.Printf("    Port: localhost:%d -> %s:%d/%s\n", p.HostPort, resp.GuestIP, p.GuestPort, p.Proto)
	}
	fmt.Printf("    Exec: mvm exec %s -- <command>\n", resp.Name)

	if startup == nil {
		return nil
	}

	// Merge attached secrets into the recipe's env, decrypted from host
	// memory here — mirrors the identical block in runStartAppleVZ below.
	if env, err := secretEnvVars(secretNames); err != nil {
		return fmt.Errorf("load secrets for startup recipe: %w", err)
	} else if len(env) > 0 {
		if startup.Env == nil {
			startup.Env = map[string]string{}
		}
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i > 0 {
				startup.Env[kv[:i]] = kv[i+1:]
			}
		}
	}

	fmt.Printf("  Waiting for guest agent before running startup recipe...\n")
	if err := waitForReady(60*time.Second, func() error {
		_, _, err := sc.Exec(ctx, name, "true")
		return err
	}); err != nil {
		return fmt.Errorf("VM %q never became ready for the startup recipe: %w", name, err)
	}

	timer := newPhaseTimer()
	logf := func(format string, a ...any) { fmt.Printf(format, a...) }
	if err := runStartupRecipe(ctx, daemonRecipeAgent{sc: sc, vmName: name}, startup, timer, logf); err != nil {
		fmt.Printf("    Startup recipe failed: %v\n", err)
		return err
	}
	return nil
}
```

`internal/cli/start.go` already imports `"context"`, `"strings"`, `"time"`, and `"github.com/agentstep/mvm/internal/server"` — no import changes needed here.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestRunStartViaDaemon|TestRunStartNoLongerRejects' -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go
git commit -m "feat(cli): support --startup/--secret on the daemon/Firecracker path"
```

---

### Task 5: Phase 1 verification

**Files:** none (verification only).

- [ ] **Step 1: Run the full module build, vet, and test suite**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: clean build, `go vet` silent, every package `ok` (hardware-dependent tests may skip; no FAILs).

- [ ] **Step 2: Confirm applevz's existing `--startup`/`--secret` behavior is unchanged**

Run: `go test ./internal/cli/ -run 'TestApplevzSpec|TestRunStartupRecipe' -v`
Expected: PASS — `applevzSpec`'s existing tests are untouched by this phase.

- [ ] **Step 3: Manual smoke test (best-effort)**

If this machine has a running daemon (`go run ./cmd/mvm init` with the Firecracker backend, or `MVM_REMOTE` pointed at a live cloud install):

```bash
mvm secret put TEST_SECRET --value hello-world
mvm start smoke-fc --secret TEST_SECRET
mvm exec smoke-fc -- sh -c 'echo $TEST_SECRET'   # expect: hello-world
mvm delete smoke-fc
mvm secret rm TEST_SECRET
```

Expected: `hello-world` printed, proving the secret value reached the guest without ever being visible in `mvm inspect smoke-fc` (only the name `TEST_SECRET` should appear there). If no daemon is available in this environment, skip and note it in the report.

- [ ] **Step 4: Commit (only if Steps 1-2 required a fix)**

If everything already passed clean, skip. Otherwise:

```bash
git add -A
git commit -m "fix: address Phase 1 full-suite verification findings"
```

---

## Phase 2: `--image` on applevz

**Key finding, grounding this entire phase:** `mvm build` (`internal/cli/build.go`) always routes through the daemon (`requireDaemon()` → `sc.Build(...)` → `handleBuild` → `firecracker.BuildRootfs(..., firecracker.CacheDir(), ...)`, `internal/server/routes.go:747-781`). `firecracker.CacheDir()` resolves to `firecracker.DataDir()/cache`, and `DataDir()` defaults to `/opt/mvm` (`internal/firecracker/config.go:12-20`) — a path inside the daemon process's own Linux filesystem. The daemon binary only ever runs on Linux (`server.IsLinux()`, `internal/server/server.go:82-84`): locally that's the Lima guest, remotely that's a cloud server (`MVM_DATA_DIR=/var/mvm` in `scripts/install-cloud.sh:383`). Lima's virtiofs mount only shares the user's **home directory** with macOS (`.mounts=[{"location":"~","writable":true}]`, `internal/lima/lima.go:168`) — `/opt/mvm` is not under `~`, so it is never shared. **A `mvm build`-produced `.ext4` therefore never appears on the macOS host filesystem at all.** applevz boots directly on macOS with no daemon involved (`runStartAppleVZ` reads `~/.mvm/cache/base.ext4` — `internal/cli/start.go:234-236`), so without an explicit fetch step, `--image` can never work on applevz no matter what CLI flag plumbing exists.

This also means `mvm build` itself is silently daemon-dependent today regardless of backend — on a pure-applevz host with no Lima/no daemon ever started, `mvm build` already fails with "daemon not running" (`build.go`'s `requireDaemon()` call has no backend check at all). This plan does not change that (building an ext4 image requires loop-mounting + chrooting, which macOS cannot do natively) — it only closes the loop for *using* an image that was built via a reachable daemon.

### Task 6: Daemon image-download endpoint + `Client.DownloadImage`

**Files:**
- Modify: `internal/server/routes.go`
- Modify: `internal/server/routes_test.go`
- Modify: `internal/server/client.go`
- Modify: `internal/server/client_test.go`

**Interfaces:**
- Produces: `func (s *Server) handleImageDownload(w http.ResponseWriter, r *http.Request)`, registered at `GET /images/{name}/download` (+ `/v1` alias via `buildMux`'s `register` helper). `func (c *Client) DownloadImage(ctx context.Context, name, destPath string) error`. Task 7 consumes `Client.DownloadImage`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/routes_test.go` (needs `"github.com/agentstep/mvm/internal/firecracker"` added to its import block):

```go
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
)
```

```go
func TestHandleImageDownload(t *testing.T) {
	s, _ := testServer(t)
	t.Setenv("MVM_DATA_DIR", t.TempDir())
	if err := os.MkdirAll(firecracker.CacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firecracker.CacheDir(), "my-image.ext4"), []byte("fake-ext4-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/images/my-image/download", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "fake-ext4-bytes" {
		t.Errorf("body = %q, want the image file's raw bytes", w.Body.String())
	}
}

func TestHandleImageDownloadNotFound(t *testing.T) {
	s, _ := testServer(t)
	t.Setenv("MVM_DATA_DIR", t.TempDir())

	req := httptest.NewRequest("GET", "/v1/images/nope/download", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
```

Append to `internal/server/client_test.go`:

```go
func TestClientDownloadImage(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/my-image/download" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte("fake-ext4-bytes"))
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	dest := filepath.Join(t.TempDir(), "cache", "my-image.ext4")
	if err := c.DownloadImage(context.Background(), "my-image", dest); err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(data) != "fake-ext4-bytes" {
		t.Errorf("downloaded content = %q, want fake-ext4-bytes", data)
	}
}

func TestClientDownloadImageNotFoundLeavesNoPartialFile(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": `image "nope" not found`})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	dest := filepath.Join(t.TempDir(), "nope.ext4")
	err := c.DownloadImage(context.Background(), "nope", dest)
	if err == nil {
		t.Fatal("DownloadImage() = nil, want an error for a missing image")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Errorf("dest %s should not exist after a failed download", dest)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestHandleImageDownload|TestClientDownloadImage' -v`
Expected: FAIL — `undefined: s.handleImageDownload` (via a 404 from an unregistered route, so the test fails on status code) and `c.DownloadImage undefined` (compile error).

- [ ] **Step 3: Write minimal implementation**

Add to `internal/server/routes.go`:

```go
// handleImageDownload streams a built custom-image file to the caller. This
// is how a custom image built via `mvm build` — which always runs on the
// daemon's own Linux host (firecracker.CacheDir(), never shared with macOS,
// see the Phase 2 finding in the backend-parity plan) — reaches an applevz
// host, which runs directly on macOS with no daemon and no shared
// filesystem with that Linux host at all.
func (s *Server) handleImageDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	imagePath := filepath.Join(firecracker.CacheDir(), name+".ext4")

	f, err := os.Open(imagePath)
	if err != nil {
		httpError(w, fmt.Errorf("image %q not found (expected %s)", name, imagePath), http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	io.Copy(w, f)
}
```

Register it in `buildMux` (alongside the other `/images` routes):

```go
	register("GET", "/images", s.handleImageList)
	register("DELETE", "/images/{name}", s.handleImageDelete)
	register("GET", "/images/{name}/download", s.handleImageDownload)
```

Add to `internal/server/client.go` (needs `"path/filepath"` added to its import block, alongside the existing `bufio`, `bytes`, `context`, `crypto/tls`, `crypto/x509`, `encoding/json`, `fmt`, `io`, `net`, `net/http`, `os`, `strings`, `sync`, `time`):

```go
// DownloadImage streams a custom image built via mvm build down to destPath.
// Writes to a temp file alongside destPath first and renames into place, so
// a failed/interrupted download never leaves a half-written image where a
// later start could pick it up.
func (c *Client) DownloadImage(ctx context.Context, name, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.url(fmt.Sprintf("/v1/images/%s/download", name)), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return fmt.Errorf("download image %q: %w", name, err)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	tmp := destPath + ".downloading"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destPath)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestHandleImageDownload|TestClientDownloadImage' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/server/routes.go internal/server/routes_test.go internal/server/client.go internal/server/client_test.go
git commit -m "feat(server): add image-download endpoint so a built image can leave the daemon's host"
```

---

### Task 7: `runStartAppleVZ` accepts `--image`, resolving or fetching it

**Files:**
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/start_test.go`
- Modify: `internal/cli/bench.go` (one call-site update)

**Interfaces:**
- Consumes: `Client.DownloadImage` (Task 6), `requireDaemon` (existing).
- Produces: `func imageFileName(image string) string`, `func resolveAppleVZImage(cacheDir, image string, fetch func(image, destPath string) error) (string, error)`. `runStartAppleVZ` gains a trailing `image string` parameter.

- [ ] **Step 1: Write the failing test**

Update `internal/cli/start_test.go`'s import block to add `"os"`:

```go
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)
```

Append:

```go
// === imageFileName / resolveAppleVZImage ===

func TestImageFileName(t *testing.T) {
	if got := imageFileName(""); got != "base.ext4" {
		t.Errorf(`imageFileName("") = %q, want base.ext4`, got)
	}
	if got := imageFileName("my-image"); got != "my-image.ext4" {
		t.Errorf(`imageFileName("my-image") = %q, want my-image.ext4`, got)
	}
}

func TestResolveAppleVZImageDefaultsToBase(t *testing.T) {
	path, err := resolveAppleVZImage("/cache", "", nil)
	if err != nil {
		t.Fatalf("resolveAppleVZImage: %v", err)
	}
	if path != "/cache/base.ext4" {
		t.Errorf("path = %q, want /cache/base.ext4", path)
	}
}

func TestResolveAppleVZImageUsesLocalCacheWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "my-image.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetchCalled := false
	path, err := resolveAppleVZImage(dir, "my-image", func(image, dest string) error {
		fetchCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("resolveAppleVZImage: %v", err)
	}
	if fetchCalled {
		t.Error("fetch was called for an already-cached image")
	}
	if path != filepath.Join(dir, "my-image.ext4") {
		t.Errorf("path = %q, want the cached path", path)
	}
}

func TestResolveAppleVZImageFetchesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	var fetchedImage, fetchedDest string
	path, err := resolveAppleVZImage(dir, "my-image", func(image, dest string) error {
		fetchedImage, fetchedDest = image, dest
		return os.WriteFile(dest, []byte("fetched"), 0o644)
	})
	if err != nil {
		t.Fatalf("resolveAppleVZImage: %v", err)
	}
	if fetchedImage != "my-image" || fetchedDest != path {
		t.Errorf("fetch(%q, %q), want (my-image, %q)", fetchedImage, fetchedDest, path)
	}
}

func TestResolveAppleVZImageErrorsWhenNoDaemonAvailable(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveAppleVZImage(dir, "my-image", nil)
	if err == nil {
		t.Fatal("resolveAppleVZImage() = nil, want an error when the image is missing and there's no way to fetch it")
	}
	if !strings.Contains(err.Error(), "my-image") {
		t.Errorf("err = %v, want it to name the missing image", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestImageFileName|TestResolveAppleVZImage' -v`
Expected: FAIL — `undefined: imageFileName`, `undefined: resolveAppleVZImage`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/cli/start.go` (near `runStartAppleVZ`):

```go
// imageFileName maps an --image value to its rootfs filename, matching the
// Firecracker path's convention (firecracker.CacheDir()+"/"+name+".ext4",
// internal/firecracker/config.go:229). image == "" means the implicit
// default.
func imageFileName(image string) string {
	if image == "" {
		return "base.ext4"
	}
	return image + ".ext4"
}

// resolveAppleVZImage returns the local rootfs path for image inside
// cacheDir, fetching it via fetch first if it isn't already cached locally.
// fetch is injected so this is testable without a real daemon; runStartAppleVZ
// passes a closure around requireDaemon()+Client.DownloadImage. A nil fetch
// with a missing image is a clear, immediate error rather than a nil-pointer
// call.
func resolveAppleVZImage(cacheDir, image string, fetch func(image, destPath string) error) (string, error) {
	rootfsPath := filepath.Join(cacheDir, imageFileName(image))
	if image == "" {
		return rootfsPath, nil
	}
	if _, err := os.Stat(rootfsPath); err == nil {
		return rootfsPath, nil
	}
	if fetch == nil {
		return "", fmt.Errorf("image %q not found in %s and no daemon reachable to fetch it (build it with: mvm build -t %s -f <Dockerfile>)", image, cacheDir, image)
	}
	if err := fetch(image, rootfsPath); err != nil {
		return "", fmt.Errorf("fetch image %q from daemon: %w", image, err)
	}
	return rootfsPath, nil
}
```

Update `runStartAppleVZ`'s signature (append `image string` as the last parameter) and its body — replace the hardcoded `rootfsPath` line:

```go
func runStartAppleVZ(store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, cpus, memoryMB int, volumes []string, out outputMode, startup *StartupSpec, secretNames []string, image string) (*BootResult, error) {
	logf := func(format string, a ...any) {
		if out == outHuman {
			fmt.Printf(format, a...)
		} else {
			fmt.Fprintf(os.Stderr, format, a...)
		}
	}
	timer := newPhaseTimer()

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".mvm", "cache")
	kernelPath := filepath.Join(cacheDir, "vmlinux")
	rootfsPath, err := resolveAppleVZImage(cacheDir, image, func(img, dest string) error {
		sc, dErr := requireDaemon()
		if dErr != nil {
			return dErr
		}
		logf("  Image %q not cached locally, fetching from daemon...\n", img)
		return sc.DownloadImage(context.Background(), img, dest)
	})
	if err != nil {
		return nil, err
	}
	timer.mark("image_resolve")

	vmDir := filepath.Join(home, ".mvm", "vms", name)
	os.MkdirAll(vmDir, 0o755)
	vmRootfs := filepath.Join(vmDir, "rootfs.ext4")
	statePath := filepath.Join(vmDir, "state.vzvmsave")
	...
```

(Everything below `statePath := ...` in the original function body is unchanged — `rootfsPath` is already used downstream, e.g. in the `cp -c %s %s` clone step, exactly as before, just now resolved dynamically instead of hardcoded to `base.ext4`.)

Update `runStart`'s applevz call site (append `, image`):

```go
	if backend == "applevz" {
		out := outHuman
		if jsonOut {
			out = outJSON
		}
		_, err := runStartAppleVZ(store, name, detach, ports, netPolicy, cpus, memoryMB, volumes, out, startup, secretNames, image)
		return err
	}
```

Update `internal/cli/bench.go`'s call site (append `, ""` — bench always uses the default image):

```go
	res, err := runStartAppleVZ(store, benchVMName, false, nil, "open", 0, 0, nil, outQuiet, nil, nil, "")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestImageFileName|TestResolveAppleVZImage' -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go internal/cli/bench.go
git commit -m "feat(cli): support --image on the applevz backend, fetching from the daemon on demand"
```

---

### Task 8: `mvm build` proactively pushes the image to the applevz cache

Closes the loop end-to-end: after Task 7, `mvm start --image` on applevz works via lazy on-demand fetch, but a user who just ran `mvm build` on an applevz host shouldn't have to know that. Best-effort — a failure here doesn't fail the build; Task 7's lazy fetch is still the safety net.

**Files:**
- Modify: `internal/cli/build.go`
- Modify: `internal/cli/root.go` (one call-site update)
- Create: `internal/cli/build_test.go`

**Interfaces:**
- Consumes: `Client.DownloadImage` (Task 6), `state.Store.GetBackend` (existing).
- Produces: `newBuildCmd` gains a `store *state.Store` parameter; `runBuild` gains a `store *state.Store` parameter.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/build_test.go`:

```go
package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

func TestRunBuildDownloadsToAppleVZCacheAfterBuild(t *testing.T) {
	dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("RUN echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	downloadHit := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/build":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"image": "my-image", "status": "built"})
		case r.URL.Path == "/v1/images/my-image/download":
			downloadHit = true
			w.Write([]byte("fake-ext4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")

	if err := runBuild(store, dockerfile, "my-image", 512); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	if !downloadHit {
		t.Error("runBuild did not fetch the built image into the applevz cache")
	}
	if _, err := os.Stat(filepath.Join(home, ".mvm", "cache", "my-image.ext4")); err != nil {
		t.Errorf("image not cached locally: %v", err)
	}
}

func TestRunBuildSkipsDownloadOnFirecrackerBackend(t *testing.T) {
	dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
	os.WriteFile(dockerfile, []byte("RUN echo hi\n"), 0o644)

	downloadHit := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/build":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"image": "my-image", "status": "built"})
		case r.URL.Path == "/v1/images/my-image/download":
			downloadHit = true
			w.Write([]byte("fake-ext4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")

	if err := runBuild(store, dockerfile, "my-image", 512); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	if downloadHit {
		t.Error("runBuild fetched the image into the applevz cache on a firecracker-backend host — should be a no-op")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRunBuild -v`
Expected: FAIL — compile error, `runBuild` doesn't take a `*state.Store` argument yet.

- [ ] **Step 3: Write minimal implementation**

Update `internal/cli/build.go`:

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newBuildCmd(store *state.Store) *cobra.Command {
	var (
		file   string
		tag    string
		sizeMB int
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build a custom rootfs image from a Dockerfile",
		Long: `Build a custom rootfs image by applying Dockerfile RUN and ENV steps
to the mvm base image. The result is cached and can be used with mvm start --image.

  mvm build -f Dockerfile -t my-image
  mvm build -f Dockerfile -t my-image --size 1024
  mvm start my-app --image my-image`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			if tag == "" {
				return fmt.Errorf("--tag (-t) is required")
			}
			return runBuild(store, file, tag, sizeMB)
		},
	}

	cmd.Flags().StringVarP(&file, "file", "f", "", "path to Dockerfile")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "image name/tag")
	cmd.Flags().IntVar(&sizeMB, "size", 512, "additional size in MiB to add to the image")

	return cmd
}

func runBuild(store *state.Store, file, imageName string, sizeMB int) error {
	steps, err := firecracker.ParseDockerfile(file)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("no supported build steps found in %s", file)
	}

	fmt.Printf("Parsed %d build step(s) from %s\n", len(steps), file)
	for i, s := range steps {
		preview := s.Args
		if len(preview) > 60 {
			preview = preview[:57] + "..."
		}
		fmt.Printf("  %d. %s %s\n", i+1, s.Directive, preview)
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	fmt.Printf("\nBuilding image '%s' (this may take several minutes)...\n", imageName)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := sc.Build(ctx, imageName, steps, sizeMB); err != nil {
		return err
	}

	fmt.Printf("  Image '%s' built successfully!\n", imageName)

	// applevz has no daemon and no filesystem in common with wherever the
	// daemon that just built this actually runs (Lima's own /opt/mvm, or a
	// remote cloud box's /var/mvm — see firecracker.CacheDir()/DataDir()).
	// Pull the freshly-built image into applevz's own cache (~/.mvm/cache)
	// right away so `mvm start --image` doesn't have to fetch it lazily on
	// first use. Best-effort: a failure here doesn't fail the build — Task 7's
	// on-demand fetch is still the safety net.
	if store != nil && store.GetBackend() == "applevz" {
		home, _ := os.UserHomeDir()
		dest := filepath.Join(home, ".mvm", "cache", imageName+".ext4")
		fmt.Printf("  Fetching '%s' into the Apple VZ image cache...\n", imageName)
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dcancel()
		if err := sc.DownloadImage(dctx, imageName, dest); err != nil {
			fmt.Printf("  Warning: could not fetch image to the Apple VZ cache yet: %v\n", err)
			fmt.Printf("  It will be fetched automatically on first `mvm start --image %s`.\n", imageName)
		}
	}

	fmt.Printf("  Use it with: mvm start <name> --image %s\n", imageName)
	return nil
}
```

In `internal/cli/root.go`, update the registration:

```go
		newBuildCmd(store),
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestRunBuild -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/build.go internal/cli/build_test.go internal/cli/root.go
git commit -m "feat(cli): mvm build proactively fetches the image into the applevz cache"
```

---

### Task 9: Relax `mvm run`'s applevz `--image` guard

`internal/cli/run.go`'s `runRun` currently hard-rejects any non-default image on applevz:

```go
	if resolvedImage != "" && store.GetBackend() == "applevz" {
		return fmt.Errorf("mvm run --image is not supported on the Apple VZ backend yet (only the default image); got %q", image)
	}
```

That guard predates Tasks 7-8 and is now factually wrong — custom images work on applevz. Its corresponding test, `TestRunRunRejectsCustomImageOnAppleVZ` (`internal/cli/run_test.go`, added by `docs/superpowers/plans/2026-07-19-mvm-run.md`'s Task 4), encodes the now-obsolete behavior and must be replaced, not left in place — leaving it would either fail outright or silently pass for the wrong reason depending on what happens to be cached on the machine running the suite.

**Files:**
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/run_test.go`

**Interfaces:** none new — this task only removes code and a stale test.

- [ ] **Step 1: Remove the guard**

In `internal/cli/run.go`'s `runRun`, delete:

```go
	// runStartAppleVZ doesn't accept an image parameter at all today — a
	// pre-existing gap in `mvm start --image` on applevz. Fail clearly here
	// rather than silently booting the default rootfs for a request that
	// named something else.
	if resolvedImage != "" && store.GetBackend() == "applevz" {
		return fmt.Errorf("mvm run --image is not supported on the Apple VZ backend yet (only the default image); got %q", image)
	}
```

`resolvedImage` now flows straight into `runStart`'s existing `image` parameter unchanged — no other line in `runRun` needs to change.

- [ ] **Step 2: Replace the obsolete test**

In `internal/cli/run_test.go`, delete `TestRunRunRejectsCustomImageOnAppleVZ` and replace it with:

```go
func TestRunRunNoLongerHardBlocksCustomImageOnAppleVZ(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")

	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "")
	// Custom images on applevz are no longer a hard-blocked feature (Phase 2
	// of docs/superpowers/plans/2026-07-19-backend-parity.md). Whatever this
	// machine's local daemon/cache state produces, the error — if any — must
	// come from actual image resolution failing, never from the old blanket
	// rejection.
	if err != nil && strings.Contains(err.Error(), "not supported on the Apple VZ backend") {
		t.Fatalf("runRun() = %v, want the old blanket applevz --image rejection to be gone", err)
	}
}
```

Add `"strings"` to `internal/cli/run_test.go`'s import block if not already present (it currently imports `"fmt"`, `"path/filepath"`, `"testing"`, `"time"`, and `"github.com/agentstep/mvm/internal/state"`).

- [ ] **Step 3: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestRunRun -v`
Expected: PASS (the new test; the old `TestRunRunRejectsCustomImageOnAppleVZ` no longer exists).

- [ ] **Step 4: Commit**

```bash
git add internal/cli/run.go internal/cli/run_test.go
git commit -m "fix(cli): mvm run no longer hard-blocks custom images on applevz"
```

---

### Task 10: Phase 2 verification

**Files:** none (verification only).

- [ ] **Step 1: Run the full module build, vet, and test suite**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: clean build, `go vet` silent, every package `ok`.

- [ ] **Step 2: Confirm the daemon/Firecracker `--image` path is unchanged**

Run: `go test ./internal/server/ -run TestHandleCreateVM -v`
Expected: PASS — `handleCreateVM`'s existing image-validation behavior (`internal/server/routes.go:191-199`) is untouched by this phase.

- [ ] **Step 3: Manual smoke test (best-effort)**

If this machine is configured with the applevz backend and has a reachable daemon (a local Lima install, even if not the active backend, works for the build step):

```bash
echo -e "FROM base\nRUN echo hello > /hello.txt" > /tmp/Dockerfile
mvm build -f /tmp/Dockerfile -t smoke-image
mvm start smoke-applevz --image smoke-image
mvm exec smoke-applevz -- cat /hello.txt   # expect: hello
mvm delete smoke-applevz
```

Expected: `hello` printed, and `~/.mvm/cache/smoke-image.ext4` exists on the macOS host. If no daemon is reachable in this environment, skip and note it in the report.

- [ ] **Step 4: Commit (only if Steps 1-2 required a fix)**

If everything already passed clean, skip. Otherwise:

```bash
git add -A
git commit -m "fix: address Phase 2 full-suite verification findings"
```

---

## Phase 3: `mvm logs` via daemon

**Scoping finding:** `internal/cli/logs.go`'s `runLogs` already has two log paths, and only one of them bypasses the daemon. The guest journal path (no `--boot`) already routes through `runExec` (`logs.go:71,79`), which — after Task 2 above — already goes through the daemon for Firecracker VMs. Only the `--boot` path (`showBootLog`, `logs.go:82-110`) shells into Lima directly via `limaClient.ShellWithTimeout`/`limaClient.ShellInteractive`, reading `firecracker.VMDir(vm.Name)/firecracker.log`. This phase only needs to replace `showBootLog`; the non-boot branch is already correct and untouched. The applevz boot-log path (`showLocalLog`, reading `~/.mvm/vms/<name>/console.log` directly — no daemon, no Lima, ever) also stays exactly as-is, preserving `logs.go`'s existing backend-split shape ("local applevz check first, then daemon" — the same shape `list.go`/`delete.go` use).

### Task 11: Daemon `GET /vms/{name}/logs` endpoint (boot logs, NDJSON streaming)

**Files:**
- Modify: `internal/server/routes.go`
- Modify: `internal/server/routes_test.go`

**Interfaces:**
- Produces: `func (s *Server) handleVMLogs(w http.ResponseWriter, r *http.Request)`, registered at `GET /vms/{name}/logs` (+ `/v1` alias). `func tailLines(f *os.File, n int) (string, error)`. Task 12 consumes this endpoint via `Client.StreamLogs`.

- [ ] **Step 1: Write the failing tests**

Add `"strconv"` to `internal/server/routes.go`'s import block later in Step 3. First, append to `internal/server/routes_test.go` (import block already has `firecracker` and `strings` from Task 6 — if implementing phases out of order, add them now):

```go
func TestTailLines(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("l1\nl2\nl3\nl4\nl5\n")
	f.Seek(0, 0)

	got, err := tailLines(f, 2)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if got != "l4\nl5\n" {
		t.Errorf("tailLines = %q, want %q", got, "l4\nl5\n")
	}
}

func TestTailLinesNoTrailingNewline(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("l1\nl2\nl3")
	f.Seek(0, 0)

	got, err := tailLines(f, 2)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if got != "l2\nl3" {
		t.Errorf("tailLines = %q, want %q", got, "l2\nl3")
	}
}

func TestHandleVMLogsRequiresBootParam(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "web", Status: "running", Backend: "firecracker", CreatedAt: time.Now()})

	req := httptest.NewRequest("GET", "/v1/vms/web/logs", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without ?boot=true", w.Code)
	}
}

func TestHandleVMLogsReturnsFileContents(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "web", Status: "running", Backend: "firecracker", CreatedAt: time.Now()})
	t.Setenv("MVM_DATA_DIR", t.TempDir())

	vmDir := firecracker.VMDir("web")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "firecracker.log"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/vms/web/logs?boot=true", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var frame struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(w.Body.Bytes()), &frame); err != nil {
		t.Fatalf("decode NDJSON frame: %v; body: %s", err, w.Body.String())
	}
	if frame.Type != "data" || frame.Data != "line1\nline2\nline3\n" {
		t.Errorf("frame = %+v, want the full log contents", frame)
	}
}

func TestHandleVMLogsTail(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "web", Status: "running", Backend: "firecracker", CreatedAt: time.Now()})
	t.Setenv("MVM_DATA_DIR", t.TempDir())

	vmDir := firecracker.VMDir("web")
	os.MkdirAll(vmDir, 0o755)
	os.WriteFile(filepath.Join(vmDir, "firecracker.log"), []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644)

	req := httptest.NewRequest("GET", "/v1/vms/web/logs?boot=true&tail=2", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	var frame struct {
		Data string `json:"data"`
	}
	json.Unmarshal(bytes.TrimSpace(w.Body.Bytes()), &frame)
	if frame.Data != "l4\nl5\n" {
		t.Errorf("tail data = %q, want the last 2 lines", frame.Data)
	}
}

func TestHandleVMLogsNotFoundVM(t *testing.T) {
	s, _ := testServer(t)
	req := httptest.NewRequest("GET", "/v1/vms/nope/logs?boot=true", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestTailLines|TestHandleVMLogs' -v`
Expected: FAIL — `undefined: tailLines`, and the `TestHandleVMLogs*` tests get 404s from an unregistered route.

- [ ] **Step 3: Write minimal implementation**

Add `"strconv"` to `internal/server/routes.go`'s import block. Append:

```go
// tailLines returns the last n lines of f (already positioned at the
// start), consuming the file. Loads the whole file into memory — boot logs
// are one VM's console output, small enough that this is simpler than a
// seek-from-end scan.
func tailLines(f *os.File, n int) (string, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	text := string(data)
	trailingNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return out, nil
}

// handleVMLogs serves a VM's Firecracker boot/console log — the file
// showBootLog (internal/cli/logs.go) used to read over limaClient.Shell().
// The daemon runs natively on the same Linux host as this file (Lima's
// guest OS locally, or a cloud server remotely — see firecracker.VMDir), so
// it opens it directly; no shell-out needed. Guest journal logs (the
// non-boot path) already go through the existing exec endpoint and are out
// of scope — this endpoint only ever serves ?boot=true.
func (s *Server) handleVMLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if r.URL.Query().Get("boot") != "true" {
		httpError(w, fmt.Errorf("this endpoint only serves boot logs (?boot=true) — guest logs go through exec"), http.StatusBadRequest)
		return
	}

	if _, err := s.store.GetVM(name); err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	tail := 0
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			tail = n
		}
	}
	follow := r.URL.Query().Get("follow") == "true"

	logPath := filepath.Join(firecracker.VMDir(name), "firecracker.log")
	f, err := os.Open(logPath)
	if err != nil {
		httpError(w, fmt.Errorf("open boot log: %w", err), http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	writeFrame := func(data string) bool {
		frame, _ := json.Marshal(map[string]string{"type": "data", "data": data})
		if _, err := w.Write(append(frame, '\n')); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	if tail > 0 {
		lines, err := tailLines(f, tail)
		if err != nil {
			httpError(w, err, http.StatusInternalServerError)
			return
		}
		if !writeFrame(lines) {
			return
		}
	} else {
		data, err := io.ReadAll(f)
		if err != nil {
			httpError(w, err, http.StatusInternalServerError)
			return
		}
		if !writeFrame(string(data)) {
			return
		}
	}

	if !follow {
		return
	}

	// Poll for appended bytes until the client disconnects. There's no
	// inotify in the stdlib and this file has a single appending writer
	// (Firecracker's own redirected stdout — config.go's
	// `>"$VM_DIR/firecracker.log"`), so a short poll loop makes the same
	// trade-off `tail -f` itself makes.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			buf := make([]byte, 4096)
			n, err := f.Read(buf)
			if n > 0 {
				if !writeFrame(string(buf[:n])) {
					return
				}
			}
			if err != nil && err != io.EOF {
				return
			}
		}
	}
}
```

Register in `buildMux`:

```go
	register("GET", "/vms/{name}", s.handleInspectVM)
	register("GET", "/vms/{name}/logs", s.handleVMLogs)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestTailLines|TestHandleVMLogs' -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/server/routes.go internal/server/routes_test.go
git commit -m "feat(server): add GET /vms/{name}/logs for boot logs, streamed as NDJSON"
```

---

### Task 12: `Client.StreamLogs`

**Files:**
- Modify: `internal/server/client.go`
- Modify: `internal/server/client_test.go`

**Interfaces:**
- Produces: `func (c *Client) StreamLogs(ctx context.Context, vmName string, tail int, follow bool, w io.Writer) error`. Task 13's `showBootLog` consumes this exactly as declared here.

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/client_test.go`:

```go
func TestClientStreamLogsNonFollow(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("boot") != "true" {
			t.Errorf("query = %s, want boot=true", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		frame, _ := json.Marshal(map[string]string{"type": "data", "data": "hello boot log\n"})
		w.Write(frame)
		w.Write([]byte("\n"))
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	var buf bytes.Buffer
	if err := c.StreamLogs(context.Background(), "web", 0, false, &buf); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if buf.String() != "hello boot log\n" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello boot log\n")
	}
}

func TestClientStreamLogsQueryParams(t *testing.T) {
	var gotQuery string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/x-ndjson")
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	var buf bytes.Buffer
	c.StreamLogs(context.Background(), "web", 50, true, &buf)
	if !strings.Contains(gotQuery, "tail=50") || !strings.Contains(gotQuery, "follow=true") {
		t.Errorf("query = %q, want tail=50 and follow=true", gotQuery)
	}
}

func TestClientStreamLogsPropagatesErrorFrame(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		frame, _ := json.Marshal(map[string]string{"type": "error", "error": "boom"})
		w.Write(frame)
		w.Write([]byte("\n"))
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	var buf bytes.Buffer
	err := c.StreamLogs(context.Background(), "web", 0, false, &buf)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to surface the error frame", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestClientStreamLogs -v`
Expected: FAIL — compile error `undefined: c.StreamLogs`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/server/client.go`:

```go
// StreamLogs fetches a VM's boot log from the daemon and writes it to w. If
// follow is true, it keeps streaming appended data until the daemon closes
// the connection or ctx is canceled (mirrors `docker logs -f`).
func (c *Client) StreamLogs(ctx context.Context, vmName string, tail int, follow bool, w io.Writer) error {
	q := "?boot=true"
	if tail > 0 {
		q += fmt.Sprintf("&tail=%d", tail)
	}
	if follow {
		q += "&follow=true"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.url(fmt.Sprintf("/v1/vms/%s/logs%s", vmName, q)), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return fmt.Errorf("logs: %w", err)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var frame struct {
			Type  string `json:"type"`
			Data  string `json:"data"`
			Error string `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			continue
		}
		if frame.Error != "" {
			return fmt.Errorf("%s", frame.Error)
		}
		if frame.Type == "data" {
			w.Write([]byte(frame.Data))
		}
	}
	return scanner.Err()
}
```

(`bufio`, `context`, `encoding/json`, `fmt`, `io`, `net/http` are already imported in `client.go` — no import changes needed.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run TestClientStreamLogs -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/server/client.go internal/server/client_test.go
git commit -m "feat(server): add Client.StreamLogs for the daemon boot-log endpoint"
```

---

### Task 13: Rewire `mvm logs --boot` to use the daemon instead of Lima

**Files:**
- Modify: `internal/cli/logs.go`
- Modify: `internal/cli/root.go` (two call-site updates)
- Modify: `internal/cli/vm.go` (one call-site update)

**Interfaces:**
- Consumes: `Client.StreamLogs` (Task 12), `requireDaemon` (existing).
- Produces: `newLogsCmd(store *state.Store)` (drops the `limaClient` parameter), `runLogs(store *state.Store, name string, follow, boot bool, tail int) error` (drops `limaClient`), `showBootLog(sc *server.Client, vm *state.VM, follow bool, tail int) error` (replaces the `limaClient`-based signature).

- [ ] **Step 1: Write the failing test**

This task is a pure rewire with no new decision logic (the new logic — NDJSON parsing, query params, tail/follow — is already covered by Task 12's `Client.StreamLogs` tests), so there's no new unit to TDD in isolation. Verify by compiling and running the existing suite before making changes, to establish the baseline:

Run: `go build ./... && go test ./internal/cli/ -run TestParsePorts -v`
Expected: PASS (baseline — confirms the package builds before this task's edits).

- [ ] **Step 2: Rewrite `internal/cli/logs.go`**

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newLogsCmd(store *state.Store) *cobra.Command {
	var (
		follow bool
		boot   bool
		tail   int
	)

	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Fetch logs from a microVM",
		Long: `Fetch logs from a microVM.

  mvm logs my-vm              # guest system log
  mvm logs my-vm -f           # follow log output
  mvm logs my-vm --boot       # kernel/boot console log
  mvm logs my-vm --boot -f    # follow boot log live
  mvm logs my-vm -n 50        # last 50 lines`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(store, args[0], follow, boot, tail)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	cmd.Flags().BoolVar(&boot, "boot", false, "show VM boot/console log")
	cmd.Flags().IntVarP(&tail, "tail", "n", 0, "number of lines from end")

	return cmd
}

func runLogs(store *state.Store, name string, follow, boot bool, tail int) error {
	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}

	if boot {
		if vm.Backend == "applevz" {
			return showLocalLog(filepath.Join(mvmDir, "vms", vm.Name, "console.log"), follow, tail)
		}
		sc, err := requireDaemon()
		if err != nil {
			return err
		}
		return showBootLog(sc, vm, follow, tail)
	}

	// Guest journal — run via exec (agent), not SSH
	if vm.Status != "running" {
		return fmt.Errorf("microVM %q is not running. Use --boot for boot logs of stopped VMs", vm.Name)
	}

	var logCmd string
	if follow {
		if tail > 0 {
			logCmd = fmt.Sprintf("tail -n %d -f /var/log/messages 2>/dev/null || dmesg -w", tail)
		} else {
			logCmd = "tail -f /var/log/messages 2>/dev/null || dmesg -w"
		}
		return runExec(store, name, []string{"sh", "-c", logCmd}, true, "", nil, "")
	}

	if tail > 0 {
		logCmd = fmt.Sprintf("tail -n %d /var/log/messages 2>/dev/null || dmesg | tail -n %d", tail, tail)
	} else {
		logCmd = "cat /var/log/messages 2>/dev/null || dmesg"
	}
	return runExec(store, name, []string{"sh", "-c", logCmd}, false, "", nil, "")
}

// showBootLog streams a Firecracker VM's boot/console log from the daemon
// (GET /vms/{name}/logs?boot=true — internal/server/routes.go's
// handleVMLogs). The daemon runs on the same Linux host as the log file, so
// this is a plain HTTP round trip; no Lima shell-out needed.
func showBootLog(sc *server.Client, vm *state.VM, follow bool, tail int) error {
	return sc.StreamLogs(context.Background(), vm.Name, tail, follow, os.Stdout)
}

func showLocalLog(logPath string, follow bool, tail int) error {
	if follow {
		args := []string{"-f", logPath}
		if tail > 0 {
			args = []string{"-n", fmt.Sprintf("%d", tail), "-f", logPath}
		}
		cmd := exec.Command("tail", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if tail > 0 {
		cmd := exec.Command("tail", "-n", fmt.Sprintf("%d", tail), logPath)
		cmd.Stdout = os.Stdout
		return cmd.Run()
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return err
	}
	fmt.Print(string(data))
	return nil
}
```

(`internal/firecracker` and `internal/lima` are no longer imported — both were only used by the old `showBootLog`.)

- [ ] **Step 3: Update call sites**

In `internal/cli/root.go`, change:

```go
		newLogsCmd(limaClient, store),
```

to:

```go
		newLogsCmd(store),
```

In `internal/cli/vm.go`, change the identical line inside `newVMCmd`'s `cmd.AddCommand(...)` call the same way. (`newVMCmd(limaClient *lima.Client, store *state.Store)`'s own signature is unchanged — `limaClient` stays an unused-but-harmless parameter there, since other subcommands registered via `internal/cli/root.go`'s `newVMCmd(limaClient, store)` call still need it in principle; Go does not error on an unused function parameter.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && go test ./internal/cli/... ./internal/server/... -v 2>&1 | tail -60`
Expected: clean build, `go vet` silent, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/logs.go internal/cli/root.go internal/cli/vm.go
git commit -m "feat(cli): mvm logs --boot reads via the daemon instead of shelling into Lima"
```

---

### Task 14: Phase 3 verification

**Files:** none (verification only).

- [ ] **Step 1: Run the full module build, vet, and test suite**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: clean build, `go vet` silent, every package `ok`.

- [ ] **Step 2: Confirm the guest-journal (non-boot) logs path is unchanged**

Run: `go test ./internal/cli/ -run TestParsePorts -v && go build ./...`
Expected: PASS — this task never touched `runExec` or the non-boot branch of `runLogs`.

- [ ] **Step 3: Manual smoke test (best-effort)**

If this machine has a running Firecracker-backend daemon with at least one VM:

```bash
mvm start smoke-logs
mvm logs smoke-logs --boot -n 20
mvm logs smoke-logs --boot -f &   # Ctrl-C after a few lines
mvm delete smoke-logs
```

Expected: boot log lines print without any `limactl`/SSH round trip (compare timing/behavior against `mvm logs smoke-logs` without `--boot`, which already went through the daemon before this plan). If no daemon is reachable, skip and note it in the report.

- [ ] **Step 4: Commit (only if Steps 1-2 required a fix)**

If everything already passed clean, skip. Otherwise:

```bash
git add -A
git commit -m "fix: address Phase 3 full-suite verification findings"
```

---

## Out of Scope (explicitly)

- **Guest-journal (non-`--boot`) logs daemon endpoint** — already routes through the daemon via `runExec` today; Phase 3 only touches the `--boot` path (see the Phase 3 scoping finding above).
- **Building applevz images without a daemon** — applying Dockerfile `RUN`/`ENV` steps to an `.ext4` image requires loop-mounting and chrooting a Linux filesystem, which macOS cannot do natively. `mvm build` remains daemon-dependent on every backend; this plan only makes the *result* reach applevz.
- **OCI image store, registry pull/push** — out of scope for the same reason it's out of scope in `docs/superpowers/plans/2026-07-19-mvm-run.md`; `--image` here still means "a name previously built with `mvm build`," not an OCI reference.
- **`mvm start --json` output for the daemon path's startup-recipe timing** (`BootResult`/`phaseTimer` JSON) — Phase 1 reuses `newPhaseTimer()` internally for `timer.mark(...)` calls but does not thread a `BootResult` back out of `runStartViaDaemon`; that function's return type stays `error`, matching its pre-existing contract. Only the applevz path (`runStartAppleVZ`) returns a `*BootResult` today.
- **Removing the 500ms polling loop in `handleVMLogs`'s follow mode for something like inotify** — the existing `firecracker.log` writer (Firecracker's own redirected stdout) has no better host-side signal available; this matches the same trade-off `tail -f` makes.
- **`newVMCmd`'s unused `limaClient` parameter** — Task 13 stops threading it into `newLogsCmd`, but leaves `newVMCmd`'s own signature alone; other subcommands under `mvm vm ...` may still gain a need for it, and Go does not error on an unused parameter.
