# Hardening & Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close four small, known gaps deferred from recent code reviews of `mvm run` (`docs/superpowers/plans/2026-07-19-mvm-run.md`): (1) an error-returning `GetBackendE()` for backend guards where a transient store-load error must not silently resolve to "firecracker"; (2) a quiet mode so `mvm run`'s foreground path no longer double-prints `runStart`'s boot banner ahead of the user's command output; (3) unit tests for two daemon-fallback branches (`existingVMNames`, `runInspect`) that currently only exercise their local-state path; (4) a `--rm` flag on `mvm run` that turns today's silent "`-d` never auto-deletes" gap into an explicit, documented decision.

**Architecture:** All four are CLI-layer or state-layer changes within the existing `internal/cli` / `internal/state` packages — no new packages, no daemon protocol changes. Tasks 1, 2, and 4 touch production code (`internal/state/store.go`, `internal/cli/run.go`, `internal/cli/start.go`, `internal/cli/bootresult.go`, `internal/cli/root.go`'s help text is untouched). Task 3 is test-only. Tasks are ordered so each builds on the previous task's signature changes in the same files — see each task's **Files** list for exact overlaps.

**Tech Stack:** Go 1.22+, cobra, stdlib-only tests (`net/http/httptest`, `os.Pipe`) — matches `internal/cli` and `internal/server`'s existing conventions. No testify, no mocking framework.

## Global Constraints

- **Gateway compat:** `mvm start <name> --net-policy deny`'s stdout output must not change one byte. Task 2's new `quiet` parameter defaults to `false` at every existing call site except `mvm run`'s own internal `runStart` call.
- **No new packages, no daemon/protocol changes.** Everything here is CLI-layer or `internal/state`-layer.
- **Test seam for "fake daemon" (Tasks 2 and 3):** point `requireDaemon()` at a fake server via `t.Setenv("MVM_REMOTE", srv.URL)` where `srv := httptest.NewServer(mux)`, plus `t.Setenv("MVM_API_KEY", "")`. This works because `server.DefaultClient()` (`internal/server/client.go:137`) checks `MVM_REMOTE` first and, when set, builds a plain `http.Transport`-backed client via `NewRemoteClient` — no TLS is forced (TLS only activates for an `https://` URL), and no `Authorization` header is sent when `apiKey == ""`. The fake server's handler is a bare `http.ServeMux`, so it never runs the real daemon's `authMiddleware` (`internal/server/auth.go`) at all — auth is a non-issue for this seam, not something to work around. `server.DefaultSocketPath()` (`internal/server/server.go:51`) has no env-var override, so the unix-socket path is not usable for this purpose; `MVM_REMOTE` is the only viable seam, and it already works today (confirmed by reading `DefaultClient`, `NewRemoteClient`, and `internal/server/client_test.go`'s existing `MVM_REMOTE` tests).
- **Danger of a real daemon on the developer's machine:** any test that wants `requireDaemon()` to fail (not succeed against a fake server) must not rely on "no daemon happens to be running" — it must force failure deterministically via `t.Setenv("MVM_REMOTE", "http://127.0.0.1:1")` (port 1: guaranteed nothing listens, connection refused near-instantly, no TLS handshake to hang on).
- Match existing code style: tabs, stdlib-only tests, matching every other file in `internal/cli` / `internal/state`.
- Repo module path is `github.com/agentstep/mvm`. Run all commands from `/Users/paulmeller/Projects/firecracker`.
- Each task is independently shippable and committable, but Tasks 2 and 4 each add a new trailing parameter to a function (`runStart`, `runRun` respectively) that later tasks' test edits assume is already in place — implement in the order given (1, 2, 3, 4) in a single working tree, or rebase carefully if parallelizing.

---

### Task 1: `GetBackendE()` — error-returning backend lookup, migrated at the one call site where it matters

**Files:**
- Modify: `internal/state/store.go`
- Modify: `internal/state/store_test.go`
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/run_test.go`

**Interfaces:**
- Produces: `func (s *Store) GetBackendE() (string, error)`. `GetBackend()` (existing) becomes a thin wrapper around it.

**Design note — why only `internal/cli/run.go` migrates:**

`grep -rn "GetBackend" internal/` finds five call sites: `internal/cli/run.go:155`, `internal/cli/start.go:137`, `internal/cli/idle.go:174`, `internal/cli/bench.go:65`, `internal/cli/doctor.go:31`. Reading each:

- **`run.go:155`** (the motivating example) — `runRun`'s applevz custom-image guard calls `store.GetBackend()` as the *first* store access in that code path, with no prior `Load()` in the same call. A transient load error here silently resolves to `"firecracker"`, which skips the guard entirely and lets a custom-image request fall through to `runStartAppleVZ` — which ignores its `image` parameter and silently boots the default rootfs instead of the one the user asked for. Real, independently-testable silent-bypass risk. **Migrate.**
- **`start.go:137`** — `runStart` calls `store.IsInitialized()` (which itself calls `Load()`) immediately before `store.GetBackend()`, synchronously, on the same file, with no write in between. By the time `GetBackend()` runs, `Load()` has already just succeeded on this exact file moments earlier in the same goroutine — a second `Load()` of the unmodified file cannot independently fail except via genuine concurrent external mutation (a real but unit-untestable TOCTOU race, not a "silent guess" in the normal case, since `IsInitialized()`'s own error already surfaces first for any ordinary corrupt-state/unreadable-file scenario). **Not migrated** — document via comment only, no behavior change.
- **`idle.go:174`** — `runIdleCheck` runs every 30s from launchd; a wrong guess here just means one pause-check cycle is skipped or run unnecessarily, self-healing on the next tick. **Not migrated** — genuinely low-stakes default.
- **`bench.go:65`** — dev-only benchmark-harness gate (`mvm bench`), not exposed to untrusted input, re-run manually each time. **Not migrated.**
- **`doctor.go:31`** — purely diagnostic printout (`Backend: firecracker`). **Not migrated.**

- [ ] **Step 1: Write the failing test (store package)**

Append to `internal/state/store_test.go`, directly after `TestGetBackendSet` (currently ending at line 413, immediately before `TestMarkInitializedWithBackend`):

```go
func TestGetBackendEDefault(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	b, err := s.GetBackendE()
	if err != nil {
		t.Fatalf("GetBackendE: %v", err)
	}
	if b != "firecracker" {
		t.Errorf("default backend = %q, want firecracker", b)
	}
}

func TestGetBackendESet(t *testing.T) {
	s := tempStore(t)
	s.MarkInitialized("v1.13.0", "applevz")

	b, err := s.GetBackendE()
	if err != nil {
		t.Fatalf("GetBackendE: %v", err)
	}
	if b != "applevz" {
		t.Errorf("backend = %q, want applevz", b)
	}
}

func TestGetBackendEPropagatesLoadError(t *testing.T) {
	s := tempStore(t)
	if err := os.WriteFile(s.Path(), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	_, err := s.GetBackendE()
	if err == nil {
		t.Fatal("GetBackendE() = nil error, want the underlying Load error surfaced")
	}
}

func TestGetBackendStillDefaultsOnLoadError(t *testing.T) {
	s := tempStore(t)
	if err := os.WriteFile(s.Path(), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	b := s.GetBackend()
	if b != "firecracker" {
		t.Errorf("GetBackend() = %q, want firecracker default even on a load error (documented behavior for low-stakes call sites)", b)
	}
}
```

`os` is already imported in `internal/state/store_test.go` (used by `TestNewStore` and friends elsewhere in the file) — no import changes needed there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/state/ -run 'TestGetBackendE|TestGetBackendStillDefaultsOnLoadError' -v`
Expected: FAIL — compile error `s.GetBackendE undefined (type *Store has no field or method GetBackendE)`.

- [ ] **Step 3: Write minimal implementation**

In `internal/state/store.go`, replace the existing `GetBackend` method (lines 309-316):

```go
// GetBackend returns the configured backend ("firecracker" or "applevz"),
// defaulting to "firecracker" both when unset AND when the state file
// fails to load (e.g. a transient I/O error). Safe for call sites where a
// wrong guess has no real consequence — a self-healing periodic check
// (idle.go), a dev-only harness gate (bench.go), or a diagnostic printout
// (doctor.go). For any call site that gates a decision with real
// consequences — skipping a validation, or dispatching to a different
// code path entirely — use GetBackendE instead, so a load error surfaces
// rather than silently resolving to "firecracker". See
// internal/cli/run.go's applevz custom-image guard for the migrated
// example, and this file's package-level notes in the hardening plan
// (docs/superpowers/plans/2026-07-19-hardening-polish.md) for why the
// other GetBackend() call sites were deliberately left as-is.
func (s *Store) GetBackend() string {
	backend, err := s.GetBackendE()
	if err != nil {
		return "firecracker" // default
	}
	return backend
}

// GetBackendE is GetBackend's error-returning counterpart: it returns the
// configured backend, or propagates the underlying Load error instead of
// papering over it with the "firecracker" default.
func (s *Store) GetBackendE() (string, error) {
	st, err := s.Load()
	if err != nil {
		return "", err
	}
	if st.Backend == "" {
		return "firecracker", nil
	}
	return st.Backend, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/state/ -run 'TestGetBackend' -v`
Expected: PASS (`TestGetBackendDefault`, `TestGetBackendSet` — pre-existing — plus the four new tests).

- [ ] **Step 5: Write the failing test (migrate run.go's guard)**

Append to `internal/cli/run_test.go`, after `TestRunRunRejectsCustomImageOnAppleVZ`:

```go
// === runRun: backend-load-error must surface, not silently default ===

func TestRunRunSurfacesBackendLoadError(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := os.WriteFile(store.Path(), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "")
	if err == nil {
		t.Fatal("runRun() = nil, want the corrupt-state load error surfaced (not silently defaulting to firecracker and booting the wrong rootfs)")
	}
}
```

Add `"os"` to `internal/cli/run_test.go`'s import block, which currently reads:

```go
import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)
```

becomes:

```go
import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRunRunSurfacesBackendLoadError -v`
Expected: FAIL — `runRun` currently calls `store.GetBackend()`, which defaults to `"firecracker"` on the corrupt file, so the guard never fires and `runRun` proceeds past it (the test fails downstream — either a nil error, or an unrelated error from later in `runRun`, not the intended coverage; the run must not silently succeed in a way that reaches `runStart` against a corrupt store).

- [ ] **Step 7: Write minimal implementation**

In `internal/cli/run.go`, replace:

```go
	// runStartAppleVZ doesn't accept an image parameter at all today — a
	// pre-existing gap in `mvm start --image` on applevz. Fail clearly here
	// rather than silently booting the default rootfs for a request that
	// named something else.
	if resolvedImage != "" && store.GetBackend() == "applevz" {
		return fmt.Errorf("custom images are not supported on the Apple VZ backend yet (only the default \"base\" image); got %q", image)
	}
```

with:

```go
	// runStartAppleVZ doesn't accept an image parameter at all today — a
	// pre-existing gap in `mvm start --image` on applevz. Fail clearly here
	// rather than silently booting the default rootfs for a request that
	// named something else. A backend-load error must surface here rather
	// than default-guessing "firecracker" (GetBackend's behavior): guessing
	// wrong would skip this guard entirely and let a custom-image request
	// silently boot the default rootfs on applevz instead. Hence
	// GetBackendE, not GetBackend — see store.go's GetBackend doc comment.
	if resolvedImage != "" {
		backend, err := store.GetBackendE()
		if err != nil {
			return fmt.Errorf("read backend: %w", err)
		}
		if backend == "applevz" {
			return fmt.Errorf("custom images are not supported on the Apple VZ backend yet (only the default \"base\" image); got %q", image)
		}
	}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestRunRun' -v && go test ./internal/state/ -v`
Expected: PASS — all `TestRunRun*` tests including the new one, and the full `internal/state` suite.

- [ ] **Step 9: Commit**

```bash
git add internal/state/store.go internal/state/store_test.go internal/cli/run.go internal/cli/run_test.go
git commit -m "fix(state): add GetBackendE and migrate mvm run's applevz-image guard to it"
```

---

### Task 2: Suppress `runStart`'s boot banner during foreground `mvm run`

**Files:**
- Modify: `internal/cli/bootresult.go`
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/run.go`
- Create: `internal/cli/start_test.go` additions (existing file, new tests)

**Interfaces:**
- Produces: `func resolveOutputMode(jsonOut, quiet bool) outputMode` (`internal/cli/bootresult.go`).
- Changes: `runStart`'s signature gains a trailing `quiet bool` parameter; `runStartViaDaemon`'s signature gains a trailing `quiet bool` parameter.

**Design:** Today, `outputMode` (`outHuman`/`outJSON`/`outQuiet`, `internal/cli/bootresult.go:7-13`) only threads through the applevz path (`runStartAppleVZ`) — the firecracker/daemon path's `runStartViaDaemon` (`internal/cli/start.go:158-187`) prints its "is running!" banner unconditionally, with no `outputMode` parameter at all (a pre-existing, separate gap: `mvm start --json` on the firecracker backend silently does nothing different — out of scope here). This task extends `outputMode`'s reach to `runStartViaDaemon` too, so quiet suppression works on both backends via one mechanism, and adds a `quiet bool` at the `runStart` call boundary (matching the existing convention there, where `--json` is already exposed as a bool `jsonOut` rather than an `outputMode` directly).

`mvm run`'s single `runStart` call site (used for both `-d` and foreground) will pass `quiet=true` unconditionally — not just for the foreground case named in the workstream. This is intentional, not scope creep: in the `-d` case, `runRun` already prints its own one-line status (`"%s (ephemeral — clean up with: mvm delete %s)\n"` or `"%s\n"`, `internal/cli/run.go:180-187`) immediately after `runStart` returns, so today's `-d` output is boot-banner-then-run's-own-line — a duplication this change also fixes, with no loss of information to the user. `mvm start`'s own call site keeps `quiet=false` always (Gateway compat).

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/start_test.go`:

```go
// === resolveOutputMode ===

func TestResolveOutputModeDefaultHuman(t *testing.T) {
	if got := resolveOutputMode(false, false); got != outHuman {
		t.Errorf("resolveOutputMode(false, false) = %v, want outHuman", got)
	}
}

func TestResolveOutputModeJSON(t *testing.T) {
	if got := resolveOutputMode(true, false); got != outJSON {
		t.Errorf("resolveOutputMode(true, false) = %v, want outJSON", got)
	}
}

func TestResolveOutputModeQuietWinsOverJSON(t *testing.T) {
	if got := resolveOutputMode(true, true); got != outQuiet {
		t.Errorf("resolveOutputMode(true, true) = %v, want outQuiet (quiet takes precedence)", got)
	}
}

// === runStart quiet mode (firecracker/daemon path) ===

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// fakeDaemonForCreate starts an httptest server implementing just enough
// of the daemon's HTTP surface (GET /health, POST /vms) for
// requireDaemon()+runStartViaDaemon to succeed against it.
func fakeDaemonForCreate(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMResponse{Name: "web", Status: "running", GuestIP: "10.0.0.2"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunStartQuietSuppressesDaemonBanner(t *testing.T) {
	srv := fakeDaemonForCreate(t)
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	out := captureStdout(t, func() {
		if err := runStart(store, "web", true, nil, "open", nil, "", "", 0, 0, "", false, nil, nil, true); err != nil {
			t.Fatalf("runStart: %v", err)
		}
	})
	if strings.Contains(out, "is running!") {
		t.Errorf("quiet runStart printed the boot banner: %q", out)
	}
}

func TestRunStartNotQuietPrintsDaemonBanner(t *testing.T) {
	srv := fakeDaemonForCreate(t)
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	out := captureStdout(t, func() {
		if err := runStart(store, "web", true, nil, "open", nil, "", "", 0, 0, "", false, nil, nil, false); err != nil {
			t.Fatalf("runStart: %v", err)
		}
	})
	if !strings.Contains(out, "is running!") {
		t.Errorf("non-quiet runStart suppressed the boot banner (Gateway compat break): %q", out)
	}
}
```

Update `internal/cli/start_test.go`'s import block from:

```go
import (
	"encoding/json"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)
```

to:

```go
import (
	"bytes"
	"encoding/json"
	"io"
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

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestResolveOutputMode|TestRunStartQuiet|TestRunStartNotQuiet' -v`
Expected: FAIL — compile errors `undefined: resolveOutputMode` and `runStart(...)` called with 15 arguments but expected 14 (the existing signature only has 14 params).

- [ ] **Step 3: Write minimal implementation**

Append to `internal/cli/bootresult.go`:

```go
// resolveOutputMode picks the outputMode a start should use. quiet takes
// precedence over jsonOut — a caller that wants no output at all (e.g.
// mvm run's foreground path, which prints its own status) never wants a
// JSON blob printed instead of the human banner it asked to suppress.
func resolveOutputMode(jsonOut, quiet bool) outputMode {
	if quiet {
		return outQuiet
	}
	if jsonOut {
		return outJSON
	}
	return outHuman
}
```

In `internal/cli/start.go`, replace the `runStart` signature and body (lines 110-154):

```go
func runStart(store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, watch string, cpus, memoryMB int, image string, jsonOut bool, startup *StartupSpec, secretNames []string) error {
	// Merge secrets from the startup spec, then validate they all exist up front
	// (a typo'd secret should fail the start, not silently inject nothing).
	if startup != nil {
		secretNames = append(secretNames, startup.Secrets...)
	}
	if err := validateSecretsExist(secretNames); err != nil {
		return err
	}

	// Cloud/remote mode: the local state doesn't matter — the daemon is
	// the source of truth. Skip the local init check entirely.
	if os.Getenv("MVM_REMOTE") != "" {
		if startup != nil || len(secretNames) > 0 {
			return fmt.Errorf("--startup/--secret are not yet supported on the daemon/firecracker path")
		}
		return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image)
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
	if startup != nil || len(secretNames) > 0 {
		return fmt.Errorf("--startup/--secret are not yet supported on the daemon/firecracker path")
	}

	// Firecracker path: route through daemon
	return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image)
}
```

with:

```go
func runStart(store *state.Store, name string, detach bool, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, watch string, cpus, memoryMB int, image string, jsonOut bool, startup *StartupSpec, secretNames []string, quiet bool) error {
	// Merge secrets from the startup spec, then validate they all exist up front
	// (a typo'd secret should fail the start, not silently inject nothing).
	if startup != nil {
		secretNames = append(secretNames, startup.Secrets...)
	}
	if err := validateSecretsExist(secretNames); err != nil {
		return err
	}

	// Cloud/remote mode: the local state doesn't matter — the daemon is
	// the source of truth. Skip the local init check entirely.
	if os.Getenv("MVM_REMOTE") != "" {
		if startup != nil || len(secretNames) > 0 {
			return fmt.Errorf("--startup/--secret are not yet supported on the daemon/firecracker path")
		}
		return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image, quiet)
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
		out := resolveOutputMode(jsonOut, quiet)
		_, err := runStartAppleVZ(store, name, detach, ports, netPolicy, cpus, memoryMB, volumes, out, startup, secretNames)
		return err
	}
	if startup != nil || len(secretNames) > 0 {
		return fmt.Errorf("--startup/--secret are not yet supported on the daemon/firecracker path")
	}

	// Firecracker path: route through daemon
	return runStartViaDaemon(name, ports, netPolicy, volumes, seccomp, cpus, memoryMB, image, quiet)
}
```

Replace `runStartViaDaemon` (lines 156-187):

```go
// runStartViaDaemon creates a VM by calling the daemon's /vms endpoint.
// Used for both local-mode (Unix socket) and cloud-mode (TCP+TLS).
func runStartViaDaemon(name string, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, cpus, memoryMB int, image string) error {
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

	return nil
}
```

with:

```go
// runStartViaDaemon creates a VM by calling the daemon's /vms endpoint.
// Used for both local-mode (Unix socket) and cloud-mode (TCP+TLS). quiet
// suppresses the boot banner entirely — used by mvm run's path, which
// prints its own status instead (see run.go). There is no JSON output
// mode on this path yet (a pre-existing, separate gap: `mvm start --json`
// on the firecracker/daemon backend silently falls back to the human
// banner, unlike the applevz path) — out of scope here; quiet only adds
// "print nothing" alongside the existing "print the human banner".
func runStartViaDaemon(name string, ports []state.PortMap, netPolicy string, volumes []string, seccomp string, cpus, memoryMB int, image string, quiet bool) error {
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
	})
	if err != nil {
		return err
	}

	if quiet {
		return nil
	}

	fmt.Printf("\n  %s is running!\n", resp.Name)
	fmt.Printf("    IP:   %s\n", resp.GuestIP)
	for _, p := range resp.Ports {
		fmt.Printf("    Port: localhost:%d -> %s:%d/%s\n", p.HostPort, resp.GuestIP, p.GuestPort, p.Proto)
	}
	fmt.Printf("    Exec: mvm exec %s -- <command>\n", resp.Name)

	return nil
}
```

Update the two existing `runStart` call sites. In `internal/cli/start.go`'s `newStartCmd` (line 65), replace:

```go
			return runStart(store, args[0], detach, portMaps, netPolicy, volumes, seccomp, watch, cpus, memoryMB, image, jsonOut, spec, secretsF)
```

with:

```go
			return runStart(store, args[0], detach, portMaps, netPolicy, volumes, seccomp, watch, cpus, memoryMB, image, jsonOut, spec, secretsF, false)
```

In `internal/cli/run.go`'s `runRun` (line 168), replace:

```go
	if err := runStart(store, name, true, ports, netPolicy, volumes, "", "", cpus, memoryMB, resolvedImage, false, nil, nil); err != nil {
```

with:

```go
	if err := runStart(store, name, true, ports, netPolicy, volumes, "", "", cpus, memoryMB, resolvedImage, false, nil, nil, true); err != nil {
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v && go build ./...`
Expected: PASS across the package (including the four new tests and all pre-existing `TestRunRun*`/`TestParsePorts`/`TestApplevzSpec*` tests unchanged), clean build.

- [ ] **Step 5: Manual smoke-check of the Gateway compat constraint**

If this machine has mvm initialized with a working backend, run:

```bash
go run ./cmd/mvm start smoke-test-vm --net-policy deny
go run ./cmd/mvm delete smoke-test-vm --force
```

Expected: identical boot-banner output to before this change (the `quiet` parameter defaults to `false` at this call site). If the backend isn't available in this environment, skip — not a blocker for the unit-tested portions of this task.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/bootresult.go internal/cli/start.go internal/cli/run.go internal/cli/start_test.go
git commit -m "fix(cli): suppress runStart's boot banner during mvm run via a quiet outputMode"
```

---

### Task 3: Daemon-branch test coverage for `existingVMNames` and `runInspect`

**Files:**
- Modify: `internal/cli/run_test.go`
- Modify: `internal/cli/inspect_test.go`

**Interfaces:** None produced — this task adds test coverage only. `existingVMNames` (`internal/cli/run.go:69-89`) and `runInspect`'s daemon-fallback branch (`internal/cli/inspect.go:24-40`) already implement the merge/fallback behavior under test (built in the original `mvm run` plan and `mvm inspect`'s applevz-split fix respectively) — there is no missing implementation, so there is no "red" step for these two tests in the usual TDD sense. Each test is expected to compile-fail first (since the fake-daemon test helpers don't exist yet) and then pass immediately once the imports/helpers are in place, not "fail, then get fixed."

**Test seam used:** `t.Setenv("MVM_REMOTE", srv.URL)` against an `httptest.NewServer` — see Global Constraints above for why this works and why the unix-socket `DefaultSocketPath()` route was ruled out (no env override exists there).

- [ ] **Step 1: Write the test for `existingVMNames`' daemon-merge branch**

Append to `internal/cli/run_test.go`, after `TestExistingVMNamesIncludesLocalApplevzVMs`:

```go
func TestExistingVMNamesMergesDaemonVMs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]server.VMResponse{
			{Name: "daemon-vm-1", Status: "running", Backend: "firecracker"},
			{Name: "daemon-vm-2", Status: "stopped", Backend: "firecracker"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.AddVM(&state.VM{Name: "local-applevz", Backend: "applevz", Status: "running", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	names, err := existingVMNames(store)
	if err != nil {
		t.Fatalf("existingVMNames: %v", err)
	}
	for _, want := range []string{"local-applevz", "daemon-vm-1", "daemon-vm-2"} {
		if !names[want] {
			t.Errorf("names = %v, want %q present (merged from local applevz state + fake daemon)", names, want)
		}
	}
}
```

Update `internal/cli/run_test.go`'s import block (already has `fmt, os, path/filepath, testing, time, github.com/agentstep/mvm/internal/state` after Tasks 1-2) to add `encoding/json, net/http, net/http/httptest, github.com/agentstep/mvm/internal/server`:

```go
import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)
```

- [ ] **Step 2: Write the test for `runInspect`'s daemon-fallback branch**

Append to `internal/cli/inspect_test.go`:

```go
// === runInspect: daemon-fallback branch ===

func TestRunInspectFallsBackToDaemon(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/vms/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{
			VMResponse: server.VMResponse{Name: r.PathValue("name"), Status: "running", Backend: "firecracker"},
			Spec:       &state.VMSpec{Cpus: 2},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	if err := runInspect(store, "daemon-vm"); err != nil {
		t.Errorf("runInspect() = %v, want nil (a VM absent from local state must fall back to the daemon)", err)
	}
}
```

Update `internal/cli/inspect_test.go`'s import block from:

```go
import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)
```

to:

```go
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)
```

- [ ] **Step 3: Run tests to verify they compile and pass**

Run: `go test ./internal/cli/ -run 'TestExistingVMNamesMergesDaemonVMs|TestRunInspectFallsBackToDaemon' -v`
Expected: PASS for both — this is coverage-only, so there is no prior-failure step to demonstrate (see the note in this task's **Interfaces** section above).

- [ ] **Step 4: Run the full package to confirm no regressions**

Run: `go test ./internal/cli/ -v`
Expected: PASS — the whole package, no regressions from the new `MVM_REMOTE`/`MVM_API_KEY` env manipulation leaking into other tests (each test uses `t.Setenv`, which Go automatically restores after the test, and `internal/cli` tests run sequentially by default — no `t.Parallel()` in this package — so there's no cross-test env race).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/run_test.go internal/cli/inspect_test.go
git commit -m "test(cli): cover existingVMNames' and runInspect's daemon-fallback branches with a fake daemon"
```

---

### Task 4: `--rm` flag — explicit, honest `-d` + auto-cleanup semantics

**Files:**
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/run_test.go`

**Interfaces:**
- Produces: `func resolveRmFlag(rm, detach bool) (warn bool, err error)`.
- Changes: `runRun`'s signature gains a trailing `rm bool` parameter; `newRunCmd` gains a `--rm` flag.

**Decision:** `mvm run -d` never auto-deletes today, silently, even when the VM is auto-named/ephemeral — there's no background process to reap it once `mvm run` returns. Rather than implement background reaping (a real feature, out of scope here — see Out of Scope), this task makes the gap loud: `--rm` combined with `-d`/`--detach` is a clear error pointing at `mvm delete`. `--rm` in foreground mode (docker muscle-memory) is accepted as an explicit no-op with a printed note, since foreground `mvm run` is already ephemeral by default unless `--name` opts into durability.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/run_test.go`:

```go
// === resolveRmFlag ===

func TestResolveRmFlagDetachErrors(t *testing.T) {
	_, err := resolveRmFlag(true, true)
	if err == nil {
		t.Fatal("resolveRmFlag(true, true) = nil error, want an error for --rm with --detach")
	}
	if !strings.Contains(err.Error(), "--rm requires a foreground command") {
		t.Errorf("error = %q, want mention of the foreground requirement", err)
	}
}

func TestResolveRmFlagForegroundWarns(t *testing.T) {
	warn, err := resolveRmFlag(true, false)
	if err != nil {
		t.Fatalf("resolveRmFlag(true, false) = %v, want nil error", err)
	}
	if !warn {
		t.Error("warn = false, want true for --rm in foreground mode")
	}
}

func TestResolveRmFlagNoRmNoWarning(t *testing.T) {
	if warn, err := resolveRmFlag(false, false); err != nil || warn {
		t.Errorf("resolveRmFlag(false, false) = (%v, %v), want (false, nil)", warn, err)
	}
	if warn, err := resolveRmFlag(false, true); err != nil || warn {
		t.Errorf("resolveRmFlag(false, true) = (%v, %v), want (false, nil)", warn, err)
	}
}

// === runRun: --rm / -d interaction ===

func TestRunRunRejectsRmWithDetach(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	err := runRun(store, "base", nil, "", true, 0, 0, "open", nil, nil, false, "", nil, "", true)
	if err == nil {
		t.Fatal("runRun() = nil, want an error for --rm with --detach")
	}
	if !strings.Contains(err.Error(), "--rm requires a foreground command") {
		t.Errorf("error = %q, want mention of the foreground requirement", err)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestRunRunRmForegroundWarnsThenContinues(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")
	// Force requireDaemon() to fail fast and deterministically, rather than
	// depending on whether this machine happens to have a real mvm daemon
	// running on the default socket (see Global Constraints).
	t.Setenv("MVM_REMOTE", "http://127.0.0.1:1")
	t.Setenv("MVM_API_KEY", "")

	stderr := captureStderr(t, func() {
		_ = runRun(store, "base", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "", true)
	})
	if !strings.Contains(stderr, "--rm has no effect in foreground mode") {
		t.Errorf("stderr = %q, want the --rm no-op warning", stderr)
	}
}
```

Update the two existing `runRun` calls that predate this task's new trailing parameter. `TestRunRunRejectsCustomImageOnAppleVZ` (added in the original `mvm run` plan):

```go
	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "")
```

becomes:

```go
	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "", false)
```

`TestRunRunSurfacesBackendLoadError` (added in Task 1):

```go
	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "")
```

becomes:

```go
	err := runRun(store, "my-custom-image", nil, "", false, 0, 0, "open", nil, nil, false, "", nil, "", false)
```

Add `"strings"` and `"bytes"` and `"io"` to `internal/cli/run_test.go`'s import block (already has `encoding/json, fmt, net/http, net/http/httptest, os, path/filepath, testing, time, server, state` after Task 3):

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestResolveRmFlag|TestRunRunRejectsRmWithDetach|TestRunRunRmForegroundWarnsThenContinues' -v`
Expected: FAIL — compile errors `undefined: resolveRmFlag` and `runRun(...)` called with the wrong number of arguments (14 vs the still-current 14-param signature not yet accepting `rm` — the two updated existing-test call sites will also fail to compile with 15 args against the old 14-param signature until Step 3 lands).

- [ ] **Step 3: Write minimal implementation**

In `internal/cli/run.go`, add above `runRun`:

```go
// resolveRmFlag validates --rm against --detach. Detached VMs can't be
// auto-reaped yet — mvm run returns and there is no background process
// watching them — so --rm -d is a clear error rather than a silent no-op.
// --rm in foreground mode is accepted (docker users reach for it out of
// muscle memory) but is genuinely redundant there: foreground mvm run is
// already ephemeral by default unless --name opts into durability. warn
// reports whether that redundancy note should be printed.
func resolveRmFlag(rm, detach bool) (warn bool, err error) {
	if rm && detach {
		return false, fmt.Errorf("--rm requires a foreground command; detached VMs can't be reaped on exit yet (clean up with: mvm delete <name>)")
	}
	return rm, nil
}
```

Replace `runRun`'s signature and opening lines:

```go
func runRun(store *state.Store, image string, cmdArgs []string, nameFlag string, detach bool, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, interactive bool, workdir string, envVars []string, user string) error {
	resolvedImage := resolveImage(image)
```

with:

```go
func runRun(store *state.Store, image string, cmdArgs []string, nameFlag string, detach bool, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, interactive bool, workdir string, envVars []string, user string, rm bool) error {
	warnRm, err := resolveRmFlag(rm, detach)
	if err != nil {
		return err
	}
	if warnRm {
		fmt.Fprintln(os.Stderr, "note: --rm has no effect in foreground mode — mvm run is already ephemeral by default unless --name is given")
	}

	resolvedImage := resolveImage(image)
```

(The later `existing, err := existingVMNames(store)` line is unchanged — `err` is already declared in this scope by `resolveRmFlag`'s call, so `:=` there continues to work exactly as before, just reusing the existing `err` variable alongside the new `existing`.)

In `newRunCmd`, add the `rm` variable to the `var` block:

```go
	var (
		name        string
		detach      bool
		cpus        int
		memoryMB    int
		netPolicy   string
		ports       []string
		volumes     []string
		interactive bool
		tty         bool
		envVars     []string
		user        string
		workdir     string
		rm          bool
	)
```

Update the `RunE` closure's call to `runRun`:

```go
			return runRun(store, image, cmdArgs, name, detach, cpus, memoryMB, netPolicy, portMaps, volumes, interactive || tty, workdir, envVars, user)
```

becomes:

```go
			return runRun(store, image, cmdArgs, name, detach, cpus, memoryMB, netPolicy, portMaps, volumes, interactive || tty, workdir, envVars, user, rm)
```

Add the flag registration alongside the others:

```go
	cmd.Flags().BoolVar(&rm, "rm", false, "detached: error, since a detached VM can't be reaped yet; foreground: no-op (mvm run is already ephemeral by default unless --name is given)")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v && go build ./...`
Expected: PASS across the package, clean build.

- [ ] **Step 5: Smoke-test the flag**

Run: `go run ./cmd/mvm run --help`
Expected: usage output includes the `--rm` flag with the help text above.

If a working backend is available:

```bash
go run ./cmd/mvm run base -d --rm
```

Expected: a clear error mentioning `--rm requires a foreground command` and `mvm delete`, and no VM created. If the backend isn't available in this environment, skip and note it — not a blocker for the unit-tested portions of this task.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/run.go internal/cli/run_test.go
git commit -m "feat(cli): add --rm to mvm run — explicit error with -d, honest no-op in foreground mode"
```

---

### Task 5: Full-suite verification

**Files:** none (verification only).

**Interfaces:** none — this task runs the existing suite, it does not add code.

- [ ] **Step 1: Run the full module build, vet, and test suite**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: clean build, `go vet` silent, every package `ok` (packages with hardware-dependent tests may skip; no FAILs).

- [ ] **Step 2: Confirm no existing command's behavior changed**

Run: `go test ./internal/cli/ -run 'TestRunStart|TestRunExec|TestRunDelete|TestParsePorts|TestApplevzSpec|TestRunInspectAppleVZDoesNotRequireDaemon' -v`
Expected: PASS — these pre-existing tests for `start`, `exec`, `delete`, `parsePorts`, `applevzSpec`, and `inspect`'s applevz path are untouched by this plan's intent and must still pass unchanged (their call sites were updated for new trailing parameters where applicable, but their assertions are identical).

Run: `go test ./internal/state/ -v`
Expected: PASS — full `internal/state` suite, including the pre-existing `TestGetBackendDefault`/`TestGetBackendSet` alongside Task 1's new `TestGetBackendE*` tests.

- [ ] **Step 3: Commit (only if Steps 1-2 required any fix)**

If everything already passed clean, there is nothing to commit here — skip. Otherwise:

```bash
git add -A
git commit -m "fix: address full-suite verification findings for hardening/polish tasks"
```

---

## Out of Scope (explicitly)

- **Migrating `store.GetBackend()` at `start.go:137`, `idle.go:174`, `bench.go:65`, `doctor.go:31`** — analyzed in Task 1's design note; each has either no independent silent-bypass risk (start.go, already gated by a preceding `IsInitialized()` `Load()`) or genuinely low stakes (idle.go, bench.go, doctor.go).
- **Wiring `--json` through the firecracker/daemon start path** — `runStartViaDaemon` still has no JSON output mode; Task 2 only adds `quiet` alongside the existing "print the human banner" default. Pre-existing gap, unrelated to this workstream.
- **Background reaping for `mvm run -d`** — Task 4 picks the cheap, honest option (a clear `--rm -d` error) over implementing a background process that watches detached ephemeral VMs and deletes them on... some trigger (exit of what, exactly, is itself undesigned). A real feature, not a hardening fix.
- **Fixing `runStartAppleVZ`'s pre-existing lack of an `image` parameter** — unrelated to Task 1's guard migration; the guard still exists specifically because this gap exists.
