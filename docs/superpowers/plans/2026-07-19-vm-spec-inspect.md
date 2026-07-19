# VM Spec Persistence + `mvm inspect` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The daemon and applevz start path persist each VM's create request as a first-class declarative spec, and a new `mvm inspect <name>` command returns it — step 1 of `docs/superpowers/specs/2026-07-19-image-vm-organization-design.md`.

**Architecture:** A new `state.VMSpec` struct is stored on `state.VM` at create time by both create paths (daemon `handleCreateVM` for Firecracker, `runStartAppleVZ` for applevz). A new `GET /vms/{name}` daemon endpoint returns `VMInspectResponse` (existing `VMResponse` + spec). All daemon routes gain `/v1/...` aliases; the new client method uses `/v1`, existing methods stay unversioned this release. The CLI `inspect` command mirrors `list`'s backend split: local store for applevz, daemon for Firecracker, both emitting the same JSON shape.

**Tech Stack:** Go 1.22+ (`net/http` ServeMux method+wildcard patterns), cobra, stdlib testing.

## Global Constraints

- **No behavior change to any existing command or endpoint** (spec "Foundation only").
- **JSON schemas are additive-only**: never remove or rename a JSON key in `VMResponse` or `state.VM` (spec "Guardrails").
- **Gateway compat**: `mvm version`, `mvm pool status`, `mvm start --net-policy deny`, `mvm exec`, `mvm delete --force`, `mvm list --json` must be observably unchanged (spec "Guardrails").
- **No image digests in this step.** `VMSpec.Image` records the reference as given; digest pinning arrives with the OCI store (design spec step 3). Do not invent a digest field.
- **Every route registered under both its legacy unversioned path and `/v1` prefix** (spec "API versioning" guardrail).
- Repo module path is `github.com/agentstep/mvm`. Run all commands from `/Users/paulmeller/Projects/firecracker`.
- Match existing code style: tabs, stdlib-only tests (no testify), table-free simple tests like `internal/server/routes_test.go`.

---

### Task 1: `state.VMSpec` type and `VM.Spec` field

**Files:**
- Create: `internal/state/spec.go`
- Modify: `internal/state/store.go` (VM struct, lines 13–36)
- Test: `internal/state/spec_test.go`

**Interfaces:**
- Consumes: existing `state.PortMap`, `state.Store` (`AddVM`, `GetVM`).
- Produces: `type VMSpec struct` with fields `Image string`, `Cpus int`, `MemoryMB int`, `Ports []PortMap`, `Volumes []string`, `NetPolicy string`, `Seccomp string`, `Secrets []string`, `Startup json.RawMessage`; and `VM.Spec *VMSpec` (JSON key `"spec"`). Later tasks depend on these exact names.

- [ ] **Step 1: Write the failing test**

Create `internal/state/spec_test.go`:

```go
package state

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestVMSpecRoundTripsThroughStore(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))

	spec := &VMSpec{
		Image:     "my-image",
		Cpus:      4,
		MemoryMB:  2048,
		Ports:     []PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
		Volumes:   []string{"/host:/guest"},
		NetPolicy: "deny",
		Seccomp:   "strict",
		Secrets:   []string{"OPENAI_API_KEY"},
		Startup:   json.RawMessage(`{"commands":["make dev"]}`),
	}
	vm := &VM{
		Name:      "web",
		Status:    "running",
		CreatedAt: time.Now(),
		Spec:      spec,
	}
	if err := store.AddVM(vm); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	got, err := store.GetVM("web")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.Spec == nil {
		t.Fatal("Spec = nil, want persisted spec")
	}
	if !reflect.DeepEqual(got.Spec, spec) {
		t.Errorf("Spec = %+v, want %+v", got.Spec, spec)
	}
}

func TestVMWithoutSpecOmitsKey(t *testing.T) {
	data, err := json.Marshal(&VM{Name: "bare"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	if _, ok := m["spec"]; ok {
		t.Error(`VM without spec should omit the "spec" key (omitempty)`)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/state/ -run TestVMSpec -v`
Expected: FAIL — compile error `undefined: VMSpec` (and `vm.Spec undefined`).

- [ ] **Step 3: Write minimal implementation**

Create `internal/state/spec.go`:

```go
package state

import "encoding/json"

// VMSpec is the declarative record of a VM create request. It is persisted
// verbatim at create time and returned by inspect — the "what the user asked
// for" companion to the runtime fields on VM. Templates and future
// declarative files serialize this same record.
type VMSpec struct {
	// Image is the reference as given at create time. Digest pinning
	// arrives with the OCI image store; no digest field until then.
	Image     string          `json:"image,omitempty"`
	Cpus      int             `json:"cpus,omitempty"`
	MemoryMB  int             `json:"memory_mb,omitempty"`
	Ports     []PortMap       `json:"ports,omitempty"`
	Volumes   []string        `json:"volumes,omitempty"`
	NetPolicy string          `json:"net_policy,omitempty"`
	Seccomp   string          `json:"seccomp,omitempty"`
	Secrets   []string        `json:"secrets,omitempty"`
	Startup   json.RawMessage `json:"startup,omitempty"`
}
```

In `internal/state/store.go`, add one field to the `VM` struct after `ForwarderPID` (line 35):

```go
	Spec         *VMSpec        `json:"spec,omitempty"` // declarative create request, returned by inspect
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/state/ -v`
Expected: PASS (all state tests, including existing store/network tests — the new field is additive).

- [ ] **Step 5: Commit**

```bash
git add internal/state/spec.go internal/state/spec_test.go internal/state/store.go
git commit -m "feat(state): add VMSpec declarative record persisted on VM"
```

---

### Task 2: Extract `buildMux` and register `/v1` route aliases

**Files:**
- Modify: `internal/server/server.go` (mux construction, lines ~120–140)
- Test: `internal/server/routes_test.go` (append)

**Interfaces:**
- Consumes: existing handler methods on `*Server` (`handleHealth`, `handleListVMs`, …).
- Produces: `func (s *Server) buildMux() *http.ServeMux` — Task 3's endpoint test and Task 7's checks serve requests through it. Every route answers on both `/path` and `/v1/path`.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/routes_test.go`:

```go
// === /v1 route aliases ===

func TestRoutesServeUnversionedAndV1(t *testing.T) {
	s, _ := testServer(t)
	mux := s.buildMux()

	for _, path := range []string{"/health", "/v1/health", "/vms", "/v1/vms"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestRoutesServeUnversionedAndV1 -v`
Expected: FAIL — compile error `s.buildMux undefined`.

- [ ] **Step 3: Implement `buildMux` and use it in `New`**

In `internal/server/server.go`, replace the inline block from `mux := http.NewServeMux()` through the last `mux.HandleFunc(...)` (currently lines 121–138) with:

```go
	mux := s.buildMux()
```

Then add this method to the same file (after the `New` function):

```go
// buildMux registers every API route twice: once at its legacy unversioned
// path (kept for existing clients per the deprecation policy) and once under
// /v1, the versioned surface new clients and SDKs target.
func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	register := func(method, path string, h http.HandlerFunc) {
		mux.HandleFunc(method+" "+path, h)
		mux.HandleFunc(method+" /v1"+path, h)
	}
	register("GET", "/health", s.handleHealth)
	register("GET", "/vms", s.handleListVMs)
	register("POST", "/vms", s.handleCreateVM)
	register("POST", "/vms/{name}/exec", s.handleExec)
	register("DELETE", "/vms/{name}", s.handleDeleteVM)
	register("POST", "/vms/{name}/stop", s.handleStopVM)
	register("POST", "/vms/{name}/pause", s.handlePauseVM)
	register("POST", "/vms/{name}/resume", s.handleResumeVM)
	register("POST", "/vms/{name}/snapshot", s.handleSnapshotCreate)
	register("POST", "/vms/{name}/restore", s.handleSnapshotRestore)
	register("GET", "/snapshots", s.handleSnapshotList)
	register("DELETE", "/snapshots/{name}", s.handleSnapshotDelete)
	register("GET", "/pool", s.handlePoolStatus)
	register("POST", "/pool/warm", s.handlePoolWarm)
	register("POST", "/build", s.handleBuild)
	register("GET", "/images", s.handleImageList)
	register("DELETE", "/images/{name}", s.handleImageDelete)
	return mux
}
```

Note: `New` assigns `s` fields before the mux lines, so `s.buildMux()` is valid at that point — verify the `s := &Server{...}` literal (line ~111) still precedes the `mux :=` line after your edit.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -v`
Expected: PASS, including the new alias test and all existing tests.

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/server/routes_test.go
git commit -m "feat(server): register all daemon routes under /v1 alongside legacy paths"
```

---

### Task 3: Daemon persists spec on create; `GET /vms/{name}` inspect endpoint

**Files:**
- Modify: `internal/server/routes.go` (`handleCreateVM` line ~134, response types block line ~61)
- Modify: `internal/server/server.go` (`buildMux`)
- Test: `internal/server/routes_test.go` (append)

**Interfaces:**
- Consumes: `state.VMSpec` (Task 1), `buildMux` (Task 2), existing `CreateVMRequest`, `VMResponse`, `testServer`.
- Produces:
  - `func specFromCreateRequest(req CreateVMRequest) *state.VMSpec`
  - `type VMInspectResponse struct { VMResponse; Spec *state.VMSpec }` with JSON key `"spec"`
  - route `GET /vms/{name}` (and `/v1/vms/{name}`) → `VMInspectResponse`; 404 JSON error for unknown names. Tasks 4 and 6 depend on `VMInspectResponse` exactly as declared here.

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/routes_test.go`:

```go
// === spec persistence + GET /vms/{name} ===

func TestSpecFromCreateRequest(t *testing.T) {
	req := CreateVMRequest{
		Name:      "web",
		Cpus:      4,
		MemoryMB:  2048,
		Ports:     []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
		NetPolicy: "deny",
		Volumes:   []string{"/h:/g"},
		Seccomp:   "strict",
		Image:     "custom",
	}
	spec := specFromCreateRequest(req)
	if spec.Image != "custom" || spec.Cpus != 4 || spec.MemoryMB != 2048 ||
		spec.NetPolicy != "deny" || spec.Seccomp != "strict" {
		t.Errorf("spec = %+v, want fields copied from request", spec)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].HostPort != 8080 {
		t.Errorf("spec.Ports = %+v, want request ports", spec.Ports)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0] != "/h:/g" {
		t.Errorf("spec.Volumes = %+v, want request volumes", spec.Volumes)
	}
}

func TestHandleInspectVM(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{
		Name:      "web",
		Status:    "running",
		GuestIP:   "10.99.0.2",
		Backend:   "firecracker",
		CreatedAt: time.Now(),
		Spec:      &state.VMSpec{Cpus: 4, NetPolicy: "deny"},
	})

	req := httptest.NewRequest("GET", "/v1/vms/web", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp VMInspectResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "web" || resp.Status != "running" {
		t.Errorf("resp = %+v, want name/status from store", resp)
	}
	if resp.Spec == nil || resp.Spec.Cpus != 4 || resp.Spec.NetPolicy != "deny" {
		t.Errorf("resp.Spec = %+v, want persisted spec", resp.Spec)
	}
}

func TestHandleInspectVMNotFound(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/vms/nope", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestSpecFromCreateRequest|TestHandleInspectVM' -v`
Expected: FAIL — compile errors `undefined: specFromCreateRequest` and `undefined: VMInspectResponse`.

- [ ] **Step 3: Implement**

In `internal/server/routes.go`, add after the `VMResponse` type (line ~70):

```go
// VMInspectResponse is VMResponse plus the persisted declarative spec.
type VMInspectResponse struct {
	VMResponse
	Spec *state.VMSpec `json:"spec,omitempty"`
}

// specFromCreateRequest records the create request as a declarative spec,
// persisted on the VM and returned by inspect.
func specFromCreateRequest(req CreateVMRequest) *state.VMSpec {
	return &state.VMSpec{
		Image:     req.Image,
		Cpus:      req.Cpus,
		MemoryMB:  req.MemoryMB,
		Ports:     req.Ports,
		Volumes:   req.Volumes,
		NetPolicy: req.NetPolicy,
		Seccomp:   req.Seccomp,
	}
}
```

In `handleCreateVM`, add one field to the `state.VM` literal (line ~134):

```go
	vm := &state.VM{
		Name:      req.Name,
		Status:    "starting",
		Ports:     req.Ports,
		NetPolicy: req.NetPolicy,
		Cpus:      req.Cpus,
		MemoryMB:  req.MemoryMB,
		CreatedAt: now,
		Spec:      specFromCreateRequest(req),
	}
```

Add the handler at the end of the handlers section (before `httpError`):

```go
func (s *Server) handleInspectVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(VMInspectResponse{
		VMResponse: VMResponse{
			Name:      vm.Name,
			Status:    vm.Status,
			GuestIP:   vm.GuestIP,
			PID:       vm.PID,
			Backend:   vm.Backend,
			Ports:     vm.Ports,
			CreatedAt: vm.CreatedAt,
		},
		Spec: vm.Spec,
	})
}
```

In `buildMux` (`internal/server/server.go`), add one line after the `GET /vms` registration:

```go
	register("GET", "/vms/{name}", s.handleInspectVM)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -v`
Expected: PASS. (`GET /vms` and `GET /vms/{name}` coexist under Go 1.22 ServeMux precedence rules.)

- [ ] **Step 5: Commit**

```bash
git add internal/server/routes.go internal/server/server.go internal/server/routes_test.go
git commit -m "feat(server): persist VM spec on create and serve GET /vms/{name} inspect"
```

---

### Task 4: `Client.InspectVM`

**Files:**
- Modify: `internal/server/client.go` (after `ListVMs`, line ~319)
- Test: `internal/server/client_test.go` (append)

**Interfaces:**
- Consumes: `VMInspectResponse` (Task 3), existing `NewClient`, `c.url`, `checkStatus`.
- Produces: `func (c *Client) InspectVM(ctx context.Context, name string) (*VMInspectResponse, error)` — Task 6 calls it exactly so. Uses the `/v1` path (a brand-new route either way, so no old-daemon skew; existing client methods stay unversioned this release and migrate per the deprecation policy).

- [ ] **Step 1: Write the failing test**

Append to `internal/server/client_test.go` (this file already imports `bufio`, `bytes`, `context`, `encoding/json`, `net`, `net/http`, `net/http/httptest`, `os`, `strings`, `testing`, `time`; add `path/filepath` and `github.com/agentstep/mvm/internal/state` to the import block):

```go
// === InspectVM ===

func TestClientInspectVM(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(VMInspectResponse{
			VMResponse: VMResponse{Name: r.PathValue("name"), Status: "running"},
			Spec:       &state.VMSpec{Cpus: 4},
		})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	c := NewClient(sock)
	got, err := c.InspectVM(context.Background(), "web")
	if err != nil {
		t.Fatalf("InspectVM: %v", err)
	}
	if got.Name != "web" || got.Status != "running" {
		t.Errorf("got = %+v, want name=web status=running", got)
	}
	if got.Spec == nil || got.Spec.Cpus != 4 {
		t.Errorf("got.Spec = %+v, want Cpus=4", got.Spec)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestClientInspectVM -v`
Expected: FAIL — compile error `c.InspectVM undefined`.

- [ ] **Step 3: Implement**

In `internal/server/client.go`, add after `ListVMs`:

```go
// InspectVM returns full details for one VM, including its declarative spec.
// New method, so it targets the versioned /v1 surface directly.
func (c *Client) InspectVM(ctx context.Context, name string) (*VMInspectResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url("/v1/vms/"+name), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	var result VMInspectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/client.go internal/server/client_test.go
git commit -m "feat(server): add Client.InspectVM against /v1"
```

---

### Task 5: applevz start path persists the spec

**Files:**
- Modify: `internal/cli/start.go` (`runStartAppleVZ` UpdateVM block, line ~316)
- Test: `internal/cli/start_test.go` (append)

**Interfaces:**
- Consumes: `state.VMSpec` (Task 1), existing `StartupSpec` (defined in `internal/cli/startup.go`, JSON-tagged), `state.PortMap`.
- Produces: `func applevzSpec(ports []state.PortMap, netPolicy string, cpus, memoryMB int, volumes []string, startup *StartupSpec, secretNames []string) *state.VMSpec`. The applevz backend has no `--image`/`--seccomp` support, so `Image` and `Seccomp` stay empty here.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/start_test.go` (ensure imports include `encoding/json` and `github.com/agentstep/mvm/internal/state`):

```go
// === applevzSpec ===

func TestApplevzSpecCapturesRequest(t *testing.T) {
	ports := []state.PortMap{{HostPort: 3000, GuestPort: 3000, Proto: "tcp"}}
	startup := &StartupSpec{Commands: []StartupCommand{{Name: "dev", Run: "make dev"}}}

	spec := applevzSpec(ports, "deny", 4, 2048, []string{"/h:/g"}, startup, []string{"KEY"})

	if spec.Cpus != 4 || spec.MemoryMB != 2048 || spec.NetPolicy != "deny" {
		t.Errorf("spec = %+v, want cpus=4 mem=2048 policy=deny", spec)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].HostPort != 3000 {
		t.Errorf("spec.Ports = %+v, want the request ports", spec.Ports)
	}
	if len(spec.Secrets) != 1 || spec.Secrets[0] != "KEY" {
		t.Errorf("spec.Secrets = %+v, want [KEY]", spec.Secrets)
	}
	var round StartupSpec
	if err := json.Unmarshal(spec.Startup, &round); err != nil {
		t.Fatalf("Startup should be valid JSON: %v", err)
	}
	if len(round.Commands) != 1 || round.Commands[0].Run != "make dev" {
		t.Errorf("Startup round-trip = %+v, want the recipe", round)
	}
}

func TestApplevzSpecNilStartup(t *testing.T) {
	spec := applevzSpec(nil, "open", 0, 0, nil, nil, nil)
	if spec.Startup != nil {
		t.Errorf("Startup = %s, want nil when no recipe given", spec.Startup)
	}
}
```

(`StartupSpec` and `StartupCommand` are declared in `internal/cli/startup.go`; `Commands` is `[]StartupCommand` with `Name`/`Run` fields — do not change those types.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestApplevzSpec -v`
Expected: FAIL — compile error `undefined: applevzSpec`.

- [ ] **Step 3: Implement**

In `internal/cli/start.go`, add near `runStartAppleVZ`:

```go
// applevzSpec records the applevz create request as a declarative spec
// (design spec §4: persisted verbatim, returned by inspect). Image and
// Seccomp stay empty — the applevz path supports neither yet.
func applevzSpec(ports []state.PortMap, netPolicy string, cpus, memoryMB int, volumes []string, startup *StartupSpec, secretNames []string) *state.VMSpec {
	spec := &state.VMSpec{
		Cpus:      cpus,
		MemoryMB:  memoryMB,
		Ports:     ports,
		Volumes:   volumes,
		NetPolicy: netPolicy,
		Secrets:   secretNames,
	}
	if startup != nil {
		if raw, err := json.Marshal(startup); err == nil {
			spec.Startup = raw
		}
	}
	return spec
}
```

Then in `runStartAppleVZ`, inside the post-boot `store.UpdateVM(name, func(v *state.VM) {` block that already sets `v.Backend = "applevz"` and `v.Secrets = secretNames` (line ~316), add:

```go
		v.Spec = applevzSpec(ports, netPolicy, cpus, memoryMB, volumes, startup, secretNames)
```

(`ports`, `netPolicy`, `cpus`, `memoryMB`, `volumes`, `startup`, `secretNames` are all parameters of `runStartAppleVZ`, in scope in that closure.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go
git commit -m "feat(cli): applevz start persists the declarative VM spec"
```

---

### Task 6: `mvm inspect` command

**Files:**
- Create: `internal/cli/inspect.go`
- Modify: `internal/cli/root.go` (command registration, line ~70)
- Test: `internal/cli/inspect_test.go`

**Interfaces:**
- Consumes: `server.VMInspectResponse` and `Client.InspectVM` (Tasks 3–4), `state.VM.Spec` (Task 1), existing `requireDaemon()` helper and `newListCmd` registration pattern.
- Produces: `mvm inspect <name>` printing one indented `VMInspectResponse` JSON object; helper `inspectResponseFromLocalVM(vm *state.VM) server.VMInspectResponse`.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/inspect_test.go`:

```go
package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestInspectResponseFromLocalVM(t *testing.T) {
	now := time.Now()
	vm := &state.VM{
		Name:      "web",
		Status:    "running",
		GuestIP:   "192.168.64.5",
		PID:       123,
		Backend:   "applevz",
		Ports:     []state.PortMap{{HostPort: 3000, GuestPort: 3000, Proto: "tcp"}},
		CreatedAt: now,
		Spec:      &state.VMSpec{Cpus: 4, NetPolicy: "deny"},
		// internal runtime fields that must NOT leak into inspect output:
		SocketPath: "/run/mvm/web.sock",
		TAPIP:      "172.16.0.1",
	}

	resp := inspectResponseFromLocalVM(vm)

	if resp.Name != "web" || resp.Status != "running" || resp.Backend != "applevz" {
		t.Errorf("resp = %+v, want identity fields copied", resp)
	}
	if resp.Spec == nil || resp.Spec.Cpus != 4 {
		t.Errorf("resp.Spec = %+v, want the VM's spec", resp.Spec)
	}

	// Schema check: the local path must emit the same shape as the daemon —
	// no state.VM internals like socket_path/tap_ip.
	data, _ := json.Marshal(resp)
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	for _, forbidden := range []string{"socket_path", "tap_ip", "tap_device", "guest_mac", "rootfs_path"} {
		if _, ok := m[forbidden]; ok {
			t.Errorf("inspect output leaks internal field %q", forbidden)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestInspectResponseFromLocalVM -v`
Expected: FAIL — compile error `undefined: inspectResponseFromLocalVM`.

- [ ] **Step 3: Implement**

Create `internal/cli/inspect.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newInspectCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Display detailed information about a microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(store, args[0])
		},
	}
}

func runInspect(store *state.Store, name string) error {
	// applevz VMs live purely in local state — the daemon has never heard
	// of them (same backend split as `mvm list`).
	if vm, err := store.GetVM(name); err == nil && vm.Backend == "applevz" {
		return printInspect(inspectResponseFromLocalVM(vm))
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	resp, err := sc.InspectVM(context.Background(), name)
	if err != nil {
		return err
	}
	return printInspect(*resp)
}

// inspectResponseFromLocalVM shapes a local state.VM into the same
// VMInspectResponse the daemon returns, so both backends emit one schema
// and internal runtime fields never leak into inspect output.
func inspectResponseFromLocalVM(vm *state.VM) server.VMInspectResponse {
	return server.VMInspectResponse{
		VMResponse: server.VMResponse{
			Name:      vm.Name,
			Status:    vm.Status,
			GuestIP:   vm.GuestIP,
			PID:       vm.PID,
			Backend:   vm.Backend,
			Ports:     vm.Ports,
			CreatedAt: vm.CreatedAt,
		},
		Spec: vm.Spec,
	}
}

func printInspect(resp server.VMInspectResponse) error {
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
```

In `internal/cli/root.go`, add to the `rootCmd.AddCommand(...)` call, directly after `newListCmd(store),` (line ~70):

```go
		newInspectCmd(store),
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v && go build ./...`
Expected: PASS and clean build.

- [ ] **Step 5: Smoke-test the command end-to-end**

Run: `go run ./cmd/mvm inspect --help`
Expected: usage showing `mvm inspect <name>`.

If a VM exists on this machine (`go run ./cmd/mvm ls`), also run `go run ./cmd/mvm inspect <that-name>` and confirm a single indented JSON object with a `"spec"` key (spec will be absent on VMs created before this change — that's correct, not a bug).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/inspect.go internal/cli/inspect_test.go internal/cli/root.go
git commit -m "feat(cli): add mvm inspect returning the VM's declarative spec"
```

---

### Task 7: JSON schema golden tests (guardrail)

**Files:**
- Create: `internal/server/schema_golden_test.go`

**Interfaces:**
- Consumes: `VMResponse`, `VMInspectResponse` (Task 3), `state.VMSpec` (Task 1).
- Produces: CI tripwire enforcing the "additive-only JSON schemas" and Gateway `list --json` guardrails from the design spec. No production code.

- [ ] **Step 1: Write the tests (these pass immediately — they are golden baselines, not TDD reds)**

Create `internal/server/schema_golden_test.go`:

```go
package server

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// These goldens enforce the design-spec guardrail (2026-07-19 image/VM
// organization): JSON schemas are additive-only. Adding a key means adding
// it to `want` here — a deliberate act. Removing or renaming a key breaks
// Gateway and SDK consumers and is forbidden by the deprecation policy.

func jsonKeys(t *testing.T, v interface{}) []string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fullVMResponse sets every field so no key is dropped by omitempty.
func fullVMResponse() VMResponse {
	return VMResponse{
		Name:      "vm",
		Status:    "running",
		GuestIP:   "10.0.0.2",
		PID:       1,
		Backend:   "firecracker",
		Ports:     []state.PortMap{{HostPort: 1, GuestPort: 2, Proto: "tcp"}},
		CreatedAt: time.Now(),
		Error:     "e",
	}
}

func TestVMResponseSchemaGolden(t *testing.T) {
	want := []string{"backend", "created_at", "error", "guest_ip", "name", "pid", "ports", "status"}
	if got := jsonKeys(t, fullVMResponse()); !reflect.DeepEqual(got, want) {
		t.Errorf("VMResponse keys = %v, want %v (additive-only: update want when adding; never remove/rename)", got, want)
	}
}

func TestVMInspectResponseSchemaGolden(t *testing.T) {
	full := VMInspectResponse{
		VMResponse: fullVMResponse(),
		Spec: &state.VMSpec{
			Image:     "i",
			Cpus:      1,
			MemoryMB:  1,
			Ports:     []state.PortMap{{HostPort: 1, GuestPort: 2, Proto: "tcp"}},
			Volumes:   []string{"v"},
			NetPolicy: "open",
			Seccomp:   "strict",
			Secrets:   []string{"s"},
			Startup:   json.RawMessage(`{}`),
		},
	}
	want := []string{"backend", "created_at", "error", "guest_ip", "name", "pid", "ports", "spec", "status"}
	if got := jsonKeys(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("VMInspectResponse keys = %v, want %v", got, want)
	}

	specWant := []string{"cpus", "image", "memory_mb", "net_policy", "ports", "seccomp", "secrets", "startup", "volumes"}
	if got := jsonKeys(t, full.Spec); !reflect.DeepEqual(got, specWant) {
		t.Errorf("VMSpec keys = %v, want %v", got, specWant)
	}
}
```

- [ ] **Step 2: Run tests to verify they pass**

Run: `go test ./internal/server/ -run SchemaGolden -v`
Expected: PASS. If a key list differs, the `want` in the test is wrong — fix the test to match the actual current schema (these are baselines of what exists, not aspirations).

- [ ] **Step 3: Run the full suite as final verification**

Run: `go test ./... 2>&1 | tail -20 && go vet ./internal/state/ ./internal/server/ ./internal/cli/`
Expected: all packages `ok` (packages with hardware-dependent tests may skip; no FAILs), vet clean.

- [ ] **Step 4: Commit**

```bash
git add internal/server/schema_golden_test.go
git commit -m "test(server): golden JSON schema guardrails for VMResponse/VMInspectResponse"
```

---

## Out of Scope (explicitly)

- Migrating existing `Client` methods to `/v1` (later release, per deprecation policy).
- Backfilling specs onto VMs created before this change (`inspect` on them shows no `"spec"` key — documented behavior).
- `mvm run`, image digests, OCI store — design-spec steps 2–3.
- A full Gateway end-to-end compat suite; Task 7's goldens cover the `list --json` schema surface Gateway consumes.
