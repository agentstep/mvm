# mvm CLI Surface Redesign — Slice 1 (Lifecycle + Output + System) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Redesign mvm's CLI to Apple `container`'s surface for the lifecycle verbs, the read/output shapes, and the `system`/`image` nouns — natively, breaking in place.

**Architecture:** CLI-layer changes only. A new presentation layer (`internal/cli/containerfmt.go`) transforms mvm's native structs into container-shaped JSON before printing; new verbs (`create`, `kill`, `start`-resume) reuse existing boot primitives; one additive daemon endpoint (`POST /vms/{name}/start`) is the only server change. The daemon HTTP API request/response structs and the `sdk/` package are frozen.

**Tech Stack:** Go 1.22+, cobra, stdlib `net/http`/`encoding/json`, **stdlib-only tests (no testify)**.

## Global Constraints

- Module path `github.com/agentstep/mvm`. Run all commands from `/Users/paulmeller/Projects/firecracker`.
- **Do NOT change** `server.VMResponse`, `server.VMStats`, `server.VMInspectResponse`, `server.CreateVMRequest`, `state.PortMap`, or anything under `sdk/` — the frozen wire/SDK contract. **New additive daemon endpoints ARE allowed** for new verbs; never alter an existing endpoint's behavior.
- **All `cf*` presentation structs live in `internal/cli/containerfmt.go`** (Task 2). Every other task imports them; none redefines them.
- Container output shapes, key names, nesting, and units follow `docs/container-compat-matrix.md` verbatim (bytes for memory/size, cumulative microseconds for CPU, JSON arrays for list/stats/inspect, single object for `system df`, empty→`[]`/`{}` never `null`).
- Flag alignment: `-c/--cpus`, `-m/--memory`, `-v/--volume` (**was `-V`**), `-p/--publish`, `-e/--env`, `--env-file`, `-d/--detach`, `-i/--interactive`, `-t/--tty`, `--name`, `--rm`, `-w/--workdir`, `-u/--user`, `-a/--all`, `-q/--quiet`, `-s/--signal`, `-t/--time`, `-f/--force`, `-t/--tag`, `-f/--file`. mvm-only flags keep their names (`--net-policy`, `--seccomp`, `--startup`, `--secret`).
- **Breaking changes are made in place** — no backward-compat aliases or dual output modes. Update `scripts/integration-test.sh` for any surface change it uses, in the same commit.
- Every command works on both backends (firecracker via `server.Client`, applevz via `state.Store` + `mvm-vz`).
- End every commit message with the repo's `Claude-Session:` trailer.

## File structure

- **Create:** `internal/cli/containerfmt.go`, `internal/cli/create.go`, `internal/cli/kill.go`, `internal/cli/image.go` (replaces `images.go`), `internal/cli/system.go`, plus `_test.go` siblings.
- **Modify:** `internal/cli/run.go`, `start.go`, `stop.go`, `delete.go`, `exec.go`, `list.go`, `inspect.go`, `stats.go`, `root.go`; `internal/firecracker/stats.go`; `internal/server/routes.go`, `server.go`, `client.go`; `scripts/integration-test.sh`.
- **Delete:** `internal/cli/images.go` (→ `image.go`); the top-level `serve`/`doctor`/`version` command registrations (logic re-homed under `system`).

## Execution order (dependency-sorted)

Tasks are numbered in execution order. Key dependencies: Task 2 (containerfmt) is foundational for all output + noun tasks; Task 3 (stop rewrite) precedes Task 4 (create calls the new `runStop`); Task 5 (daemon start endpoint) + Task 6 (applevz resume) precede Task 7 (wire `start`→resume); Task 1 (free the `-v` shorthand) precedes any task registering `-v/--volume` (Tasks 4, 9).

| Task | What | Source label |
|---|---|---|
| 1 | Free `-v` from `--verbose` (prerequisite) | new |
| 2 | `containerfmt.go` structs + transforms | B1 |
| 3 | `stop` flags (`-s/-t/-a`) + `delete` shorthands | A7 |
| 4 | `create` verb | A1 |
| 5 | Daemon `POST /vms/{name}/start` | A2 |
| 6 | applevz `start`-resume guards | A3 |
| 7 | Wire `mvm start <name>` → resume | A4 |
| 8 | `kill` verb | A5 |
| 9 | `run` persist-by-default + flag align | A6 |
| 10 | `list` running-only + container JSON | B2 |
| 11 | `inspect` container array | B3 |
| 12 | `stats` cfStats + cumulative CPU | B4 |
| 13 | `exec` without `--` | B5 |
| 14 | `image` noun (rename + ls/rm + json) | C1 |
| 15 | `image inspect` | C2 |
| 16 | `image prune` | C3 |
| 17 | `system` noun scaffold | C4 |
| 18 | `system status` | C5 |
| 19 | `system df` | C6 |

---

### Task 1: Free the `-v` shorthand from `--verbose`

The contract assigns `-v` to `--volume` on `run`/`create`, but `internal/cli/root.go` currently binds `-v` to the persistent `--verbose` flag. Cobra errors at command construction on a duplicate shorthand, so `--verbose` must become long-only **before** any subcommand registers `-v/--volume`.

**Files:**
- Modify: `internal/cli/root.go`
- Test: `internal/cli/root_test.go`

**Interfaces:**
- Produces: a root command whose persistent `--verbose` flag has no shorthand. Tasks 4 and 9 rely on `-v` being free for `--volume`.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/root_test.go` (create the file if absent, `package cli`):

```go
func TestVerboseHasNoShorthand(t *testing.T) {
	cmd := newRootCmd("test", "test", "test")
	f := cmd.PersistentFlags().Lookup("verbose")
	if f == nil {
		t.Fatal("--verbose flag missing")
	}
	if f.Shorthand != "" {
		t.Errorf("--verbose shorthand = %q, want none (freed for --volume)", f.Shorthand)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestVerboseHasNoShorthand -v`
Expected: FAIL — shorthand is currently `v`.

- [ ] **Step 3: Change the registration**

In `internal/cli/root.go`, change the persistent-flag registration from the `BoolVarP(..., "verbose", "v", ...)` form to the no-shorthand form:

```go
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "verbose output")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestVerboseHasNoShorthand -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/root.go internal/cli/root_test.go
git commit -m "refactor(cli): free -v shorthand from --verbose for --volume"
```

---

### Task 2: `containerfmt.go` — presentation structs + pure transforms

This is source label **B1**. It creates `internal/cli/containerfmt.go` with all `cf*` structs (verbatim from the plan's Global Constraints / compat matrix) and the pure transform functions consumed by Tasks 10–19.

**Files:**
- Create: `internal/cli/containerfmt.go`
- Test: `internal/cli/containerfmt_test.go`

**Interfaces:**
- Produces: `func toCFContainer(vm server.VMResponse, spec *state.VMSpec, inspect bool) cfContainer`; `func toCFContainers(vms []server.VMResponse, specs map[string]*state.VMSpec) []cfContainer`; `func toCFStats(src []cfStatSource) []cfStats`; the `cfStatSource` intermediate; and the struct types `cfContainer/cfConfiguration/cfImageRef/cfResources/cfPlatform/cfPort/cfNetwork/cfStats/cfImage/cfDescriptor/cfDiskEntry/cfDiskUsage`. Tasks 10/11 consume `toCFContainer(s)`; Task 12 consumes `toCFStats`/`cfStatSource`; Tasks 14–19 consume `cfImage`/`cfDescriptor`/`cfDiskUsage`/`cfDiskEntry`.
- Consumes: `server.VMResponse`, `state.VMSpec`, `state.PortMap` (read-only).

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/containerfmt_test.go`:

```go
package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestToCFContainersListShape(t *testing.T) {
	vms := []server.VMResponse{{
		Name:    "web",
		Status:  "running",
		GuestIP: "192.168.64.5",
		Ports:   []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
	}}
	specs := map[string]*state.VMSpec{"web": {Image: "nginx", Cpus: 2, MemoryMB: 512}}

	got := mustJSON(t, toCFContainers(vms, specs))
	want := `[
  {
    "configuration": {
      "id": "web",
      "image": {
        "reference": "nginx"
      },
      "resources": {
        "cpus": 2,
        "memoryInBytes": 536870912
      },
      "publishedPorts": [
        {
          "hostPort": 8080,
          "proto": "tcp"
        }
      ]
    },
    "status": "running",
    "networks": [
      {
        "ipv4Address": "192.168.64.5"
      }
    ]
  }
]`
	if got != want {
		t.Fatalf("list shape mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToCFContainersEmptyIsArrayNotNull(t *testing.T) {
	if got := mustJSON(t, toCFContainers(nil, nil)); got != "[]" {
		t.Fatalf("empty list: got %q want []", got)
	}
}

func TestToCFContainerDefaultImageAndNoNetwork(t *testing.T) {
	got := toCFContainer(server.VMResponse{Name: "x", Status: "stopped"}, nil, false)
	if got.Configuration.Image.Reference != "base" {
		t.Fatalf("default image: got %q want base", got.Configuration.Image.Reference)
	}
	if got.Configuration.Resources.MemoryInBytes != 0 {
		t.Fatalf("nil spec memory: got %d want 0", got.Configuration.Resources.MemoryInBytes)
	}
	if len(got.Networks) != 0 {
		t.Fatalf("no-ip networks: got %d want 0", len(got.Networks))
	}
	if got.Configuration.Platform != nil {
		t.Fatalf("list path must not set platform")
	}
}

func TestToCFContainerInspectAddsPlatformAndStartedDate(t *testing.T) {
	vm := server.VMResponse{
		Name:      "web",
		Status:    "running",
		GuestIP:   "192.168.64.5",
		CreatedAt: time.Unix(1700000000, 0),
	}
	got := mustJSON(t, []cfContainer{toCFContainer(vm, &state.VMSpec{Image: "nginx", Cpus: 1, MemoryMB: 256}, true)})
	want := `[
  {
    "configuration": {
      "id": "web",
      "image": {
        "reference": "nginx"
      },
      "resources": {
        "cpus": 1,
        "memoryInBytes": 268435456
      },
      "publishedPorts": [],
      "platform": {
        "os": "linux",
        "architecture": "arm64"
      }
    },
    "status": "running",
    "networks": [
      {
        "ipv4Address": "192.168.64.5"
      }
    ],
    "startedDate": 721692800
  }
]`
	if got != want {
		t.Fatalf("inspect shape mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToCFStatsShape(t *testing.T) {
	src := []cfStatSource{{
		Name:             "web",
		CPUUsageUsec:     12500000,
		MemoryUsageBytes: 104857600,
		MemoryLimitBytes: 536870912,
		NumProcesses:     1,
		Status:           "running",
	}}
	got := mustJSON(t, toCFStats(src))
	want := `[
  {
    "id": "web",
    "cpuUsageUsec": 12500000,
    "memoryUsageBytes": 104857600,
    "memoryLimitBytes": 536870912,
    "numProcesses": 1
  }
]`
	if got != want {
		t.Fatalf("stats shape mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToCFStatsEmptyIsArrayNotNull(t *testing.T) {
	if got := mustJSON(t, toCFStats(nil)); got != "[]" {
		t.Fatalf("empty stats: got %q want []", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestToCF -v`
Expected: FAIL — undefined types/functions.

- [ ] **Step 3: Create `internal/cli/containerfmt.go`**

```go
package cli

import (
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

// Container-CLI-compatible presentation shapes. The CLI transforms mvm's native
// structs into these before printing under --format json / inspect, so tooling
// built for Apple `container` consumes mvm's native output unchanged.

type cfPort struct {
	HostPort int    `json:"hostPort"`
	Proto    string `json:"proto"`
}
type cfImageRef struct {
	Reference string `json:"reference"`
}
type cfResources struct {
	Cpus          int   `json:"cpus"`
	MemoryInBytes int64 `json:"memoryInBytes"`
}
type cfPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}
type cfConfiguration struct {
	ID             string      `json:"id"`
	Image          cfImageRef  `json:"image"`
	Resources      cfResources `json:"resources"`
	PublishedPorts []cfPort    `json:"publishedPorts"`
	Platform       *cfPlatform `json:"platform,omitempty"` // inspect only
}
type cfNetwork struct {
	IPv4Address string `json:"ipv4Address"`
}
type cfContainer struct {
	Configuration cfConfiguration `json:"configuration"`
	Status        string          `json:"status"`
	Networks      []cfNetwork     `json:"networks"`
	StartedDate   float64         `json:"startedDate,omitempty"` // inspect only
}
type cfStats struct {
	ID               string `json:"id"`
	CPUUsageUsec     uint64 `json:"cpuUsageUsec"` // cumulative microseconds, monotonic
	MemoryUsageBytes uint64 `json:"memoryUsageBytes"`
	MemoryLimitBytes uint64 `json:"memoryLimitBytes"`
	NumProcesses     uint32 `json:"numProcesses"`
}
type cfDescriptor struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}
type cfImage struct {
	Reference  string       `json:"reference"`
	Descriptor cfDescriptor `json:"descriptor"`
}
type cfDiskEntry struct {
	Active      uint64 `json:"active"`
	Reclaimable uint64 `json:"reclaimable"`
	SizeInBytes uint64 `json:"sizeInBytes"`
	Total       uint64 `json:"total"`
}
type cfDiskUsage struct {
	Containers cfDiskEntry `json:"containers"`
	Images     cfDiskEntry `json:"images"`
	Volumes    cfDiskEntry `json:"volumes"`
}

// cfEpochOffset converts a Unix timestamp to Apple's CoreFoundation epoch
// (seconds since 2001-01-01 UTC), the base container's startedDate uses.
const cfEpochOffset = 978307200

const (
	cfOS   = "linux"
	cfArch = "arm64"
)

// cfStatSource is the CLI-side, pre-transform per-VM stats record. It exists
// because server.VMStats (a frozen wire contract that must not change) carries
// only an instantaneous %cpu, whereas cfStats needs cumulative CPU microseconds,
// byte units, a memory limit, and a process count. Status/Backend/PID are
// display-only (the human table) and intentionally absent from cfStats JSON.
type cfStatSource struct {
	Name             string
	CPUUsageUsec     uint64
	MemoryUsageBytes uint64
	MemoryLimitBytes uint64
	NumProcesses     uint32
	Status           string
	Backend          string
	PID              int
}

func imageRef(spec *state.VMSpec) string {
	if spec == nil || spec.Image == "" {
		return "base"
	}
	return spec.Image
}

func cfResourcesFrom(spec *state.VMSpec) cfResources {
	if spec == nil {
		return cfResources{}
	}
	return cfResources{
		Cpus:          spec.Cpus,
		MemoryInBytes: int64(spec.MemoryMB) * 1024 * 1024,
	}
}

func cfPortsFrom(ports []state.PortMap) []cfPort {
	out := make([]cfPort, 0, len(ports))
	for _, p := range ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		out = append(out, cfPort{HostPort: p.HostPort, Proto: proto})
	}
	return out
}

func cfNetworksFrom(guestIP string) []cfNetwork {
	if guestIP == "" {
		return []cfNetwork{}
	}
	return []cfNetwork{{IPv4Address: guestIP}}
}

// toCFContainer converts one native VMResponse (plus its persisted spec, which
// carries the image/cpus/memory that VMResponse omits) into container's
// cfContainer shape. spec may be nil. When inspect is true the inspect-only
// fields (platform, startedDate) are populated.
func toCFContainer(vm server.VMResponse, spec *state.VMSpec, inspect bool) cfContainer {
	c := cfContainer{
		Configuration: cfConfiguration{
			ID:             vm.Name,
			Image:          cfImageRef{Reference: imageRef(spec)},
			Resources:      cfResourcesFrom(spec),
			PublishedPorts: cfPortsFrom(vm.Ports),
		},
		Status:   vm.Status,
		Networks: cfNetworksFrom(vm.GuestIP),
	}
	if inspect {
		c.Configuration.Platform = &cfPlatform{OS: cfOS, Architecture: cfArch}
		if !vm.CreatedAt.IsZero() {
			c.StartedDate = float64(vm.CreatedAt.UTC().Unix() - cfEpochOffset)
		}
	}
	return c
}

// toCFContainers transforms a list of native VMs into cfContainers (list path,
// no platform/startedDate). Always non-nil so the empty case marshals to `[]`.
func toCFContainers(vms []server.VMResponse, specs map[string]*state.VMSpec) []cfContainer {
	out := make([]cfContainer, 0, len(vms))
	for _, vm := range vms {
		out = append(out, toCFContainer(vm, specs[vm.Name], false))
	}
	return out
}

// toCFStats transforms CLI-local stat sources into the flat container cfStats
// shape. Non-nil slice for the empty case.
func toCFStats(src []cfStatSource) []cfStats {
	out := make([]cfStats, 0, len(src))
	for _, s := range src {
		out = append(out, cfStats{
			ID:               s.Name,
			CPUUsageUsec:     s.CPUUsageUsec,
			MemoryUsageBytes: s.MemoryUsageBytes,
			MemoryLimitBytes: s.MemoryLimitBytes,
			NumProcesses:     s.NumProcesses,
		})
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestToCF -v && go vet ./internal/cli/`
Expected: PASS, vet silent.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/containerfmt.go internal/cli/containerfmt_test.go
git commit -m "feat(cli): container-shaped cf* presentation structs + pure transforms"
```

---

### Task 3: `stop` flags (`-s`/`-t`/`-a`) + `delete` shorthands (source A7)

**Files:**
- Modify: `internal/cli/stop.go`, `internal/cli/delete.go`
- Test: `internal/cli/stop_test.go` (new), `internal/cli/delete_test.go`

**Interfaces:**
- Produces: `func runStop(store *state.Store, name, signalName string, timeoutSec int) error` (new signature — Task 4's `create` calls it as `runStop(store, name, "TERM", 5)`); `func signalIsKill(name string) bool`; `func runStopAll(store *state.Store, signalName string, timeoutSec int) error`; `newStopCmd` with `-s/--signal` (default `TERM`), `-t/--time` (default 5), `-a/--all`; `newDeleteCmd` `--force`/`--all` gain `-f`/`-a` shorthands.
- Consumes: `Client.StopVM(ctx, name, force bool)`, `vm_pkg.NewAppleVZBackend(mvmDir).StopVM`, `killForwarder`, `localApplevzVMs`, `mergeVMResponses`, `requireDaemon`.

Mapping (frozen wire contract): `stop` is graceful by default (`-s TERM`); `-s KILL`/`9` maps to the daemon's existing force-stop (`StopVM(force=true)`). `-t/--time` is honored on the applevz teardown but advisory on Firecracker (no wire field). Replaces stop's old `--force` in place.

- [ ] **Step 1: Write the failing tests** — `internal/cli/stop_test.go`:

```go
package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestSignalIsKill(t *testing.T) {
	cases := map[string]bool{
		"KILL": true, "SIGKILL": true, "9": true, "kill": true,
		"TERM": false, "SIGTERM": false, "15": false, "": false,
	}
	for in, want := range cases {
		if got := signalIsKill(in); got != want {
			t.Errorf("signalIsKill(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewStopCmdFlags(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newStopCmd(store)
	sig := cmd.Flags().Lookup("signal")
	if sig == nil || sig.Shorthand != "s" || sig.DefValue != "TERM" {
		t.Errorf("--signal = %+v, want shorthand s default TERM", sig)
	}
	tm := cmd.Flags().Lookup("time")
	if tm == nil || tm.Shorthand != "t" || tm.DefValue != "5" {
		t.Errorf("--time = %+v, want shorthand t default 5", tm)
	}
	all := cmd.Flags().Lookup("all")
	if all == nil || all.Shorthand != "a" {
		t.Errorf("--all = %+v, want shorthand a", all)
	}
	if cmd.Flags().Lookup("force") != nil {
		t.Error("--force should be gone from stop (use -s KILL)")
	}
}

func TestRunStopAppleVZNotRunning(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	if err := store.AddVM(&state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}
	if err := runStop(store, "box", "TERM", 5); err == nil {
		t.Fatal("runStop() = nil, want a \"not running\" error")
	}
}
```

Add to `internal/cli/delete_test.go`:

```go
func TestNewDeleteCmdFlagShorthands(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newDeleteCmd(store)
	if f := cmd.Flags().Lookup("force"); f == nil || f.Shorthand != "f" {
		t.Errorf("--force = %+v, want shorthand f", f)
	}
	if a := cmd.Flags().Lookup("all"); a == nil || a.Shorthand != "a" {
		t.Errorf("--all = %+v, want shorthand a", a)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestSignalIsKill|TestNewStopCmd|TestRunStopAppleVZ|TestNewDeleteCmd' -v`
Expected: FAIL (undefined symbols / missing shorthands).

- [ ] **Step 3: Rewrite `internal/cli/stop.go`**

```go
package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newStopCmd(store *state.Store) *cobra.Command {
	var (
		signalName string
		timeout    int
		all        bool
	)

	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Gracefully stop a running microVM",
		Long: `Stop a running microVM. Sends --signal (default TERM) and waits up to
--time seconds before force-killing.

  mvm stop mybox
  mvm stop mybox -s KILL     # skip graceful shutdown, kill immediately
  mvm stop mybox -t 10       # wait up to 10s before killing
  mvm stop --all`,
		Args: func(cmd *cobra.Command, args []string) error {
			allFlag, _ := cmd.Flags().GetBool("all")
			if allFlag {
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("requires exactly 1 argument (or --all)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return runStopAll(store, signalName, timeout)
			}
			return runStop(store, args[0], signalName, timeout)
		},
	}

	cmd.Flags().StringVarP(&signalName, "signal", "s", "TERM", "signal to send (TERM graceful, KILL immediate)")
	cmd.Flags().IntVarP(&timeout, "time", "t", 5, "seconds to wait for graceful stop before killing")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "stop all running microVMs")

	return cmd
}

// signalIsKill reports whether a --signal value means "kill immediately".
func signalIsKill(name string) bool {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "KILL", "9":
		return true
	default:
		return false
	}
}

// runStop stops one VM. signalName selects graceful (TERM) vs immediate (KILL);
// timeoutSec is the graceful grace period (honored on applevz; advisory on the
// Firecracker daemon path, whose wire contract exposes only a force bool).
func runStop(store *state.Store, name, signalName string, timeoutSec int) error {
	force := signalIsKill(signalName)

	vm, _ := store.GetVM(name)
	if vm != nil && vm.Backend == "applevz" {
		if vm.Status != "running" && vm.Status != "paused" {
			return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
		}
		fmt.Printf("Stopping microVM '%s'...\n", name)
		vzBackend := vm_pkg.NewAppleVZBackend(mvmDir)
		if err := vzBackend.StopVM(name, vm.PID); err != nil {
			fmt.Printf("  Warning: %v\n", err)
		}
		killForwarder(store, name, vm.ForwarderPID)
		now := time.Now()
		store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
		fmt.Println("  ✓ VM stopped")
		return nil
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	fmt.Printf("Stopping microVM '%s'...\n", name)
	if err := sc.StopVM(context.Background(), name, force); err != nil {
		return err
	}
	if force {
		fmt.Println("  ✓ Force killed")
	} else {
		fmt.Println("  ✓ VM stopped")
	}
	return nil
}

func runStopAll(store *state.Store, signalName string, timeoutSec int) error {
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	var daemonVMs []server.VMResponse
	if sc, err := requireDaemon(); err == nil {
		if vms, err := sc.ListVMs(context.Background()); err == nil {
			daemonVMs = vms
		}
	}
	vms := mergeVMResponses(localVMs, daemonVMs)
	stopped := 0
	for _, vm := range vms {
		if vm.Status != "running" && vm.Status != "paused" {
			continue
		}
		if err := runStop(store, vm.Name, signalName, timeoutSec); err != nil {
			fmt.Printf("  Warning: failed to stop %s: %v\n", vm.Name, err)
			continue
		}
		stopped++
	}
	if stopped == 0 {
		fmt.Println("No running microVMs to stop.")
	}
	return nil
}
```

- [ ] **Step 4: `delete` shorthands** — in `internal/cli/delete.go` `newDeleteCmd`:

```go
	cmd.Flags().BoolVarP(&force, "force", "f", false, "stop the VM first if running")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "delete all microVMs")
```

- [ ] **Step 5: Verify + update integration script**

Run: `go test ./internal/cli/ -run 'TestSignalIsKill|TestNewStopCmd|TestRunStop|TestNewDeleteCmd' -v && go build ./...`
Then: `grep -nE "stop .*--force" scripts/integration-test.sh || echo "no stop --force usage"` — if found, replace with `-s KILL`.
Expected: PASS, clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/stop.go internal/cli/stop_test.go internal/cli/delete.go internal/cli/delete_test.go
git commit -m "feat(cli): stop gets -s/-t/-a; delete gains -f/-a shorthands"
```

---

### Task 4: `create` verb — provision and leave stopped (source A1)

**Files:**
- Create: `internal/cli/create.go`
- Test: `internal/cli/create_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Produces: `func newCreateCmd(store *state.Store) *cobra.Command`; `func runCreate(store *state.Store, name, image string, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, seccomp string) error`.
- Consumes: `runStart(store, name, detach, ports, netPolicy, volumes, seccomp, watch, cpus, memoryMB, image, jsonOut, startup, secretNames, quiet)` (existing), `runStop` (**Task 3** signature), `resolveImage`, `existingVMNames`, `parsePorts`, `parseVolumes` (existing).

**Approach (investigated):** neither backend has a provision-without-boot path (`handleCreateVM` and `runStartAppleVZ` both boot). So `create` honestly **boots then stops** — `runStart` (create path, `quiet=true`, `detach=true`), then `runStop` to park it `stopped`. Rootfs clone + net allocation persist.

- [ ] **Step 1: Write the failing tests** — `internal/cli/create_test.go`:

```go
package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestRunCreateRejectsExistingName(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	if err := store.AddVM(&state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}
	err := runCreate(store, "box", "base", 0, 0, "open", nil, nil, "")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runCreate() = %v, want an \"already exists\" error", err)
	}
}

func TestNewCreateCmdFlags(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newCreateCmd(store)
	for _, f := range []struct{ long, short string }{
		{"cpus", "c"}, {"memory", "m"}, {"volume", "v"}, {"publish", "p"},
	} {
		fl := cmd.Flags().Lookup(f.long)
		if fl == nil {
			t.Errorf("--%s not registered", f.long)
			continue
		}
		if fl.Shorthand != f.short {
			t.Errorf("--%s shorthand = %q, want %q", f.long, fl.Shorthand, f.short)
		}
	}
	if cmd.Flags().Lookup("image") == nil {
		t.Error("--image not registered")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestRunCreate|TestNewCreateCmd' -v`
Expected: FAIL — undefined `runCreate`/`newCreateCmd`.

- [ ] **Step 3: Implement `internal/cli/create.go`**

```go
package cli

import (
	"fmt"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newCreateCmd(store *state.Store) *cobra.Command {
	var (
		image     string
		cpus      int
		memoryMB  int
		netPolicy string
		ports     []string
		volumes   []string
		seccomp   string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Provision a microVM and leave it stopped",
		Long: `Provision a microVM (allocate config, prepare rootfs) and leave it stopped.

The VM is booted once to lay down its rootfs and network allocation, then
stopped — start it later with: mvm start <name>.

  mvm create mybox
  mvm create mybox --image my-image -c 4 -m 2048
  mvm create web -p 8080:80 -v ./src:/app`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			portMaps, err := parsePorts(ports)
			if err != nil {
				return err
			}
			vols, err := parseVolumes(volumes)
			if err != nil {
				return err
			}
			return runCreate(store, args[0], image, cpus, memoryMB, netPolicy, portMaps, vols, seccomp)
		},
	}

	cmd.Flags().StringVar(&image, "image", "", "custom rootfs image name; \"base\" or empty = default rootfs")
	cmd.Flags().IntVarP(&cpus, "cpus", "c", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVarP(&memoryMB, "memory", "m", 0, "RAM in MiB (default: 1024)")
	cmd.Flags().StringVar(&netPolicy, "net-policy", "open", "network policy: open, deny, or allow:domain1,domain2")
	cmd.Flags().StringArrayVarP(&ports, "publish", "p", nil, "publish port (hostPort:guestPort[/proto])")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "v", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().StringVar(&seccomp, "seccomp", "", "seccomp profile: strict, moderate, or permissive")

	return cmd
}

// runCreate provisions a VM and leaves it stopped. There is no
// create-without-boot path on either backend, so create honestly boots then
// stops — the rootfs clone and net allocation persist, and the VM is parked
// "stopped" ready for `mvm start`.
func runCreate(store *state.Store, name, image string, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, seccomp string) error {
	existing, err := existingVMNames(store)
	if err != nil {
		return err
	}
	if existing[name] {
		return fmt.Errorf("microVM %q already exists", name)
	}
	if err := runStart(store, name, true, ports, netPolicy, volumes, seccomp, "", cpus, memoryMB, resolveImage(image), false, nil, nil, true); err != nil {
		return fmt.Errorf("create %q: %w", name, err)
	}
	if err := runStop(store, name, "TERM", 5); err != nil {
		return fmt.Errorf("create %q: provisioned but failed to stop: %w", name, err)
	}
	fmt.Printf("%s (created, stopped)\n", name)
	return nil
}
```

- [ ] **Step 4: Register** in `internal/cli/root.go`, after `newStartCmd(store),`:

```go
		newStartCmd(store),
		newCreateCmd(store),
```

- [ ] **Step 5: Verify**

Run: `go test ./internal/cli/ -run 'TestRunCreate|TestNewCreateCmd' -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/create.go internal/cli/create_test.go internal/cli/root.go
git commit -m "feat(cli): add create verb — provision a VM and leave it stopped"
```

---

### Task 5: Daemon `POST /vms/{name}/start` — resume a stopped Firecracker VM (source A2)

**Files:**
- Modify: `internal/server/routes.go` (extract `postBootSetup` from `handleCreateVM`; add `handleStartVM`), `internal/server/server.go` (register route), `internal/server/client.go` (`Client.StartVM`)
- Test: `internal/server/routes_test.go`

**Interfaces:**
- Produces: `func (s *Server) handleStartVM(w http.ResponseWriter, r *http.Request)`; `func (s *Server) postBootSetup(name string, alloc state.NetAllocation, volumes []string, seccomp string) error`; `func (c *Client) StartVM(ctx context.Context, name string) (*VMResponse, error)`. **Task 7** consumes `Client.StartVM`.
- Consumes: `firecracker.StartExisting(ex, name, alloc, cpus, memMB) (int, error)`, `state.AllocateNet`, `firecracker.SocketPath/VMDir/WaitForGuest/SetupGuestNetworkViaAgent/SetupPortForwarding/ApplyNetworkPolicyViaAgent/SetupVolumeMounts/ApplySeccompViaAgent`.

- [ ] **Step 1: Write the failing tests** — append to `internal/server/routes_test.go`:

```go
func TestHandleStartVMResumesStopped(t *testing.T) {
	s, store := testServer(t)
	s.executor = &mockExecutor{runFunc: func(command string) (string, error) {
		return "PID: 4242", nil // StartExisting parses "PID:" from the start script
	}}
	vm := &state.VM{Name: "box", Status: "stopped", Cpus: 2, MemoryMB: 1024, CreatedAt: time.Now()}
	if _, err := store.ReserveVM(vm); err != nil {
		t.Fatalf("ReserveVM: %v", err)
	}
	req := httptest.NewRequest("POST", "/vms/box/start", nil)
	req.SetPathValue("name", "box")
	w := httptest.NewRecorder()
	s.handleStartVM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp VMResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "running" || resp.PID != 4242 {
		t.Errorf("resp = %+v, want status=running pid=4242", resp)
	}
	got, _ := store.GetVM("box")
	if got.Status != "running" {
		t.Errorf("persisted status = %q, want running", got.Status)
	}
}

func TestHandleStartVMRejectsRunning(t *testing.T) {
	s, store := testServer(t)
	if _, err := store.ReserveVM(&state.VM{Name: "box", Status: "running", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("ReserveVM: %v", err)
	}
	req := httptest.NewRequest("POST", "/vms/box/start", nil)
	req.SetPathValue("name", "box")
	w := httptest.NewRecorder()
	s.handleStartVM(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for a non-stopped VM", w.Code)
	}
}

func TestHandleStartVMNotFound(t *testing.T) {
	s, _ := testServer(t)
	req := httptest.NewRequest("POST", "/vms/ghost/start", nil)
	req.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	s.handleStartVM(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/server/ -run TestHandleStartVM -v`
Expected: FAIL — `handleStartVM` undefined.

- [ ] **Step 3: Extract `postBootSetup`** — in `handleCreateVM`, replace the inline `postBoot` closure body with a call, and add the method above `handleCreateVM`:

```go
	postBoot := func() error {
		return s.postBootSetup(req.Name, alloc, req.Volumes, req.Seccomp)
	}
```

```go
// postBootSetup runs the guest-agent-dependent setup shared by create and
// start-resume: wait for the agent, configure guest networking, then apply
// port forwarding, network policy, volume copy-in, and seccomp from the
// persisted spec. Returns the volume copy-in error if any; network/policy/
// seccomp failures are logged but non-fatal (matching the prior inline behavior).
func (s *Server) postBootSetup(name string, alloc state.NetAllocation, volumes []string, seccomp string) error {
	if !firecracker.WaitForGuest(s.executor, alloc.GuestIP, 120*time.Second) {
		log.Printf("VM %s: guest agent not reachable after 120s", name)
		return fmt.Errorf("guest agent not reachable after 120s")
	}
	firecracker.SetupGuestNetworkViaAgent(s.executor, alloc.GuestIP, alloc.TAPIP)

	postVM, err := s.store.GetVM(name)
	if err != nil {
		log.Printf("VM %s: failed to reload state for post-boot setup: %v", name, err)
		return err
	}
	if err := firecracker.SetupPortForwarding(s.executor, postVM); err != nil {
		log.Printf("VM %s: port forwarding setup failed: %v", name, err)
	}
	if err := firecracker.ApplyNetworkPolicyViaAgent(s.executor, postVM); err != nil {
		log.Printf("VM %s: network policy setup failed: %v", name, err)
	}
	var volErr error
	if len(volumes) > 0 {
		if err := firecracker.SetupVolumeMounts(postVM, volumes); err != nil {
			log.Printf("VM %s: volume mount setup failed: %v", name, err)
			volErr = err
		}
	}
	if seccomp != "" {
		if err := firecracker.ApplySeccompViaAgent(s.executor, postVM, seccomp); err != nil {
			log.Printf("VM %s: seccomp setup failed: %v", name, err)
		}
	}
	return volErr
}
```

- [ ] **Step 4: Add `handleStartVM`** (after `handleCreateVM`):

```go
// handleStartVM boots an existing STOPPED Firecracker VM in place (cold reboot,
// disk preserved). Additive endpoint for the start-resume verb — never alters
// create. Reuses the VM's existing NetIndex (no ReserveVM), boots the existing
// rootfs via StartExisting, then re-runs the shared post-boot setup.
func (s *Server) handleStartVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "stopped" {
		httpError(w, fmt.Errorf("VM %q is %s, not stopped", name, vm.Status), http.StatusConflict)
		return
	}
	alloc := state.AllocateNet(vm.NetIndex)
	pid, err := firecracker.StartExisting(s.executor, name, alloc, vm.Cpus, vm.MemoryMB)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = alloc.TAPDev
		v.GuestMAC = alloc.GuestMAC
		v.SocketPath = firecracker.SocketPath(name)
		v.PID = pid
		v.RootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"
		v.StoppedAt = nil
	})
	started, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	var volumes []string
	var seccomp string
	if started.Spec != nil {
		volumes = started.Spec.Volumes
		seccomp = started.Spec.Seccomp
	}
	postBoot := func() error { return s.postBootSetup(name, alloc, volumes, seccomp) }
	if len(volumes) > 0 {
		if err := postBoot(); err != nil {
			httpError(w, fmt.Errorf("volume setup: %w", err), http.StatusInternalServerError)
			return
		}
	} else {
		go func() { _ = postBoot() }()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(VMResponse{
		Name:      name,
		Status:    "running",
		GuestIP:   alloc.GuestIP,
		PID:       pid,
		Backend:   started.Backend,
		Ports:     started.Ports,
		CreatedAt: started.CreatedAt,
	})
}
```

- [ ] **Step 5: Register** in `internal/server/server.go` `buildMux`, after the stop route:

```go
	register("POST", "/vms/{name}/stop", s.handleStopVM)
	register("POST", "/vms/{name}/start", s.handleStartVM)
```

- [ ] **Step 6: `Client.StartVM`** in `internal/server/client.go` (after `StopVM`):

```go
// StartVM boots an existing stopped VM in place (cold reboot, disk preserved).
func (c *Client) StartVM(ctx context.Context, name string) (*VMResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.url(fmt.Sprintf("/vms/%s/start", name)), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, fmt.Errorf("start failed: %s", err)
	}
	var result VMResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
```

- [ ] **Step 7: Verify**

Run: `go test ./internal/server/ -run 'TestHandleStartVM|TestHandleCreateVM' -v && go build ./...`
Expected: new tests pass; existing `TestHandleCreateVM*` still pass (refactor preserved behavior).

- [ ] **Step 8: Commit**

```bash
git add internal/server/routes.go internal/server/server.go internal/server/client.go internal/server/routes_test.go
git commit -m "feat(server): POST /vms/{name}/start — resume a stopped Firecracker VM"
```

---

### Task 6: applevz `start`-resume — cold-boot a stopped VM (source A3)

**Files:**
- Modify: `internal/cli/start.go` (two guards in `runStartAppleVZ`)
- Test: `internal/cli/start_test.go`

**Interfaces:** no signature changes. Consumes `store.GetVM/UpdateVM/ReserveVM`, `killForwarder`, `execLocal`.

Two guards to relax: (1) the branch that hard-errors `already exists` when the VM is in state but has no `state.vzvmsave` snapshot; (2) the `cp base→rootfs` overwrite that would destroy a stopped VM's disk.

- [ ] **Step 1: Write the failing test** — append to `internal/cli/start_test.go`:

```go
func TestStoppedApplevzVMPassesExistenceGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := state.NewStore(filepath.Join(home, ".mvm", "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	vm := &state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}
	if _, err := store.ReserveVM(vm); err != nil {
		t.Fatalf("ReserveVM: %v", err)
	}
	vmDir := filepath.Join(home, ".mvm", "vms", "box")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "rootfs.ext4"), []byte("PRESERVED"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	_, err := runStartAppleVZ(store, "box", true, nil, "open", 0, 0, nil, outQuiet, nil, nil, "")
	if err != nil && strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runStartAppleVZ() = %v, want the stopped VM allowed through as a resume", err)
	}
	data, readErr := os.ReadFile(filepath.Join(vmDir, "rootfs.ext4"))
	if readErr != nil {
		t.Fatalf("read rootfs after resume attempt: %v", readErr)
	}
	if string(data) != "PRESERVED" {
		t.Errorf("rootfs = %q, want the preserved contents untouched", data)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run TestStoppedApplevzVMPassesExistenceGuard -v`
Expected: FAIL (guard rejects, or the cp overwrites `PRESERVED`).

- [ ] **Step 3: Relax Guard 1** — replace the existing-VM branch in `runStartAppleVZ`:

```go
	now := time.Now()
	var netIndex int
	if existing, _ := store.GetVM(name); existing != nil {
		// Already in state. Allow it through as either (a) a restore from a
		// saved RAM snapshot, or (b) a cold-boot resume of a cleanly stopped
		// VM (rootfs preserved, no snapshot). Anything else — a VM state
		// thinks is still running/paused with no snapshot — is a real collision.
		_, statErr := os.Stat(statePath)
		if statErr != nil && existing.Status != "stopped" {
			return nil, fmt.Errorf("microVM %q already exists", name)
		}
		netIndex = existing.NetIndex
		killForwarder(store, name, existing.ForwarderPID)
		store.UpdateVM(name, func(v *state.VM) { v.Status = "starting" })
	} else {
```

- [ ] **Step 4: Guard 2 (skip cp when rootfs exists)** — replace the `else if execLocal("cp -c ...")` branch:

```go
	} else if _, statErr := os.Stat(vmRootfs); statErr == nil {
		// A stopped VM being resumed: its per-VM rootfs already holds the disk
		// writes from before the stop. Do NOT re-copy base.ext4 over it — that
		// would destroy the preserved state (same invariant as the restore branch).
	} else if err := execLocal(fmt.Sprintf("cp -c %s %s", rootfsPath, vmRootfs)); err != nil {
		if err := execLocal(fmt.Sprintf("cp %s %s", rootfsPath, vmRootfs)); err != nil {
			store.RemoveVM(name)
			return nil, fmt.Errorf("copy rootfs: %w", err)
		}
	}
```

- [ ] **Step 5: Verify**

Run: `go test ./internal/cli/ -run TestStoppedApplevzVMPassesExistenceGuard -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go
git commit -m "fix(cli): applevz start-resume — cold-boot a stopped VM, preserve its rootfs"
```

---

### Task 7: Wire `mvm start <name>` to the resume path (source A4)

**Files:**
- Modify: `internal/cli/start.go` (`runStartViaDaemon` routes stopped→resume; extract a banner helper)
- Test: `internal/cli/start_test.go`

**Interfaces:**
- Consumes: `Client.StartVM(ctx, name)` (**Task 5**), `Client.InspectVM(ctx, name)` (existing, 404→not-found). applevz half is delivered by **Task 6** (`runStart` already dispatches applevz to `runStartAppleVZ`, which now resumes).

- [ ] **Step 1: Write the failing tests** — append to `internal/cli/start_test.go`:

```go
func TestRunStartViaDaemonResumesStopped(t *testing.T) {
	var startCalled, createCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/vms/box", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{
			VMResponse: server.VMResponse{Name: "box", Status: "stopped", Backend: "firecracker"},
		})
	})
	mux.HandleFunc("POST /vms/box/start", func(w http.ResponseWriter, r *http.Request) {
		startCalled = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMResponse{Name: "box", Status: "running", GuestIP: "10.0.0.5"})
	})
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		createCalled = true
		w.WriteHeader(http.StatusConflict)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")
	if err := runStartViaDaemon("box", nil, "open", nil, "", 0, 0, "", nil, nil, false); err != nil {
		t.Fatalf("runStartViaDaemon() = %v, want nil", err)
	}
	if !startCalled {
		t.Error("resume endpoint POST /vms/box/start was not called")
	}
	if createCalled {
		t.Error("create endpoint POST /vms was called — start should resume, not create")
	}
}

func TestRunStartViaDaemonCreatesFreshName(t *testing.T) {
	var createCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/vms/newbox", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		createCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(server.VMResponse{Name: "newbox", Status: "running", GuestIP: "10.0.0.6"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")
	if err := runStartViaDaemon("newbox", nil, "open", nil, "", 0, 0, "", nil, nil, false); err != nil {
		t.Fatalf("runStartViaDaemon() = %v, want nil", err)
	}
	if !createCalled {
		t.Error("create endpoint POST /vms was not called for a fresh name")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestRunStartViaDaemon(Resumes|Creates)' -v`
Expected: FAIL — `runStartViaDaemon` always creates.

- [ ] **Step 3: Extract the banner helper** in `internal/cli/start.go`:

```go
// printDaemonRunning prints the human "VM is running" banner from a daemon
// VMResponse. Shared by the create and resume paths of runStartViaDaemon.
func printDaemonRunning(resp *server.VMResponse) {
	fmt.Printf("\n  %s is running!\n", resp.Name)
	fmt.Printf("    IP:   %s\n", resp.GuestIP)
	for _, p := range resp.Ports {
		host := p.HostIP
		if host == "" {
			host = "localhost"
		}
		fmt.Printf("    Port: %s:%d -> %s:%d/%s\n", host, p.HostPort, resp.GuestIP, p.GuestPort, p.Proto)
	}
	fmt.Printf("    Exec: mvm exec %s <command>\n", resp.Name)
}
```

- [ ] **Step 4: Route stopped→resume** at the top of `runStartViaDaemon`, after `requireDaemon()` succeeds and before `sc.CreateVM`:

```go
	ctx := context.Background()

	// Resume path: an existing STOPPED VM cold-boots in place rather than
	// 409ing on a second create. Any other status (or not-found) falls through
	// to CreateVM. start does not accept create-time config on resume — the VM
	// boots from its persisted spec (see handleStartVM).
	if existing, ierr := sc.InspectVM(ctx, name); ierr == nil && existing.Status == "stopped" {
		resp, serr := sc.StartVM(ctx, name)
		if serr != nil {
			return serr
		}
		if quiet {
			return nil
		}
		printDaemonRunning(resp)
		return nil
	}
```

- [ ] **Step 5: Replace the inline create banner** (the block after the create path's `if quiet { return nil }`) with `printDaemonRunning(resp)`, deleting the old inline `fmt.Printf` banner lines. Leave the startup-recipe block unchanged.

- [ ] **Step 6: Verify**

Run: `go test ./internal/cli/ -run 'TestRunStartViaDaemon' -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go
git commit -m "feat(cli): mvm start <name> resumes a stopped VM instead of 409ing"
```

---

### Task 8: `kill` verb — immediate signal, both backends (source A5)

**Files:**
- Create: `internal/cli/kill.go`
- Test: `internal/cli/kill_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Produces: `func newKillCmd(store *state.Store) *cobra.Command`; `func parseSignal(name string) syscall.Signal`; `func runKill(store *state.Store, name, signalName string) error`; `func runKillAll(store *state.Store, signalName string) error`.
- Consumes: `store.GetVM/UpdateVM`, `killForwarder`, `Client.StopVM(ctx, name, force=true)`, `localApplevzVMs`, `mergeVMResponses`, `requireDaemon`.

Mapping: applevz signals the helper directly (`--signal` honored); Firecracker force-kills (SIGKILL) via `StopVM(force=true)` — non-KILL signals aren't plumbed across the frozen wire contract. Default signal KILL.

- [ ] **Step 1: Write the failing tests** — `internal/cli/kill_test.go`:

```go
package cli

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestParseSignal(t *testing.T) {
	cases := []struct {
		in   string
		want syscall.Signal
	}{
		{"KILL", syscall.SIGKILL}, {"SIGKILL", syscall.SIGKILL}, {"9", syscall.SIGKILL},
		{"TERM", syscall.SIGTERM}, {"sigterm", syscall.SIGTERM}, {"INT", syscall.SIGINT},
		{"bogus", syscall.SIGKILL},
	}
	for _, c := range cases {
		if got := parseSignal(c.in); got != c.want {
			t.Errorf("parseSignal(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRunKillAppleVZNotRunning(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	if err := store.AddVM(&state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}
	err := runKill(store, "box", "KILL")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("runKill() = %v, want a \"not running\" error", err)
	}
}

func TestNewKillCmdFlags(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newKillCmd(store)
	sig := cmd.Flags().Lookup("signal")
	if sig == nil || sig.Shorthand != "s" || sig.DefValue != "KILL" {
		t.Errorf("--signal = %+v, want shorthand s default KILL", sig)
	}
	all := cmd.Flags().Lookup("all")
	if all == nil || all.Shorthand != "a" {
		t.Errorf("--all = %+v, want shorthand a", all)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestParseSignal|TestRunKill|TestNewKillCmd' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement `internal/cli/kill.go`**

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newKillCmd(store *state.Store) *cobra.Command {
	var (
		signalName string
		all        bool
	)
	cmd := &cobra.Command{
		Use:   "kill <name>",
		Short: "Send a signal to a microVM immediately",
		Long: `Kill a microVM immediately — no graceful shutdown.

On applevz the signal is delivered to the VM helper process, so --signal is
honored. On Firecracker the daemon force-kills (SIGKILL) regardless of --signal.

  mvm kill mybox
  mvm kill mybox -s TERM
  mvm kill --all`,
		Args: func(cmd *cobra.Command, args []string) error {
			allFlag, _ := cmd.Flags().GetBool("all")
			if allFlag {
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("requires exactly 1 argument (or --all)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return runKillAll(store, signalName)
			}
			return runKill(store, args[0], signalName)
		},
	}
	cmd.Flags().StringVarP(&signalName, "signal", "s", "KILL", "signal to send (applevz only — FC always SIGKILLs)")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "kill all running microVMs")
	return cmd
}

// parseSignal maps a signal name (with/without SIG prefix, or a number) to a
// syscall.Signal. Unknown values default to SIGKILL — kill means "now".
func parseSignal(name string) syscall.Signal {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "KILL", "9":
		return syscall.SIGKILL
	case "TERM", "15":
		return syscall.SIGTERM
	case "INT", "2":
		return syscall.SIGINT
	case "HUP", "1":
		return syscall.SIGHUP
	case "QUIT", "3":
		return syscall.SIGQUIT
	default:
		return syscall.SIGKILL
	}
}

func runKill(store *state.Store, name, signalName string) error {
	if vm, _ := store.GetVM(name); vm != nil && vm.Backend == "applevz" {
		if vm.Status != "running" && vm.Status != "paused" {
			return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
		}
		sig := parseSignal(signalName)
		if vm.PID > 0 {
			if proc, err := os.FindProcess(vm.PID); err == nil {
				_ = proc.Signal(sig)
			}
		}
		killForwarder(store, name, vm.ForwarderPID)
		now := time.Now()
		store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
		fmt.Printf("  ✓ Killed %s\n", name)
		return nil
	}
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	if err := sc.StopVM(context.Background(), name, true); err != nil {
		return err
	}
	fmt.Printf("  ✓ Killed %s\n", name)
	return nil
}

func runKillAll(store *state.Store, signalName string) error {
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	var daemonVMs []server.VMResponse
	if sc, err := requireDaemon(); err == nil {
		if vms, err := sc.ListVMs(context.Background()); err == nil {
			daemonVMs = vms
		}
	}
	vms := mergeVMResponses(localVMs, daemonVMs)
	killed := 0
	for _, vm := range vms {
		if vm.Status != "running" && vm.Status != "paused" {
			continue
		}
		if err := runKill(store, vm.Name, signalName); err != nil {
			fmt.Printf("  Warning: failed to kill %s: %v\n", vm.Name, err)
			continue
		}
		killed++
	}
	if killed == 0 {
		fmt.Println("No running microVMs to kill.")
	}
	return nil
}
```

- [ ] **Step 4: Register** in `internal/cli/root.go`, after `newStopCmd(store),`:

```go
		newStopCmd(store),
		newKillCmd(store),
```

- [ ] **Step 5: Verify**

Run: `go test ./internal/cli/ -run 'TestParseSignal|TestRunKill|TestNewKillCmd' -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/kill.go internal/cli/kill_test.go internal/cli/root.go
git commit -m "feat(cli): add kill verb — immediate signal on both backends"
```

---

### Task 9: `run` persist-by-default + flag alignment (source A6)

**Files:**
- Modify: `internal/cli/run.go`
- Test: `internal/cli/run_test.go`

**Interfaces:**
- Produces: `func resolveRmFlag(rm, detach bool) (autoDelete bool, err error)` (new meaning); `func resolveRunName(nameFlag string, existing map[string]bool) string`; updated `runRun`/`newRunCmd`.
- Depends on **Task 1** (the `-v` shorthand must be free before `run` registers `-v/--volume`).

Semantics: `run` now **persists by default**; `--rm` triggers delete-on-exit; `--rm` + `-d` is an error. Flags align: add `-c/--cpus`, `-m/--memory`; `-V`→`-v`.

- [ ] **Step 1: Update tests** — in `internal/cli/run_test.go`, replace the `resolveRmFlag` tests and add name/flag tests:

```go
func TestResolveRmFlagDetachErrors(t *testing.T) {
	_, err := resolveRmFlag(true, true)
	if err == nil || !strings.Contains(err.Error(), "--rm requires a foreground command") {
		t.Fatalf("resolveRmFlag(true, true) err = %v, want the foreground-requirement error", err)
	}
}

func TestResolveRmFlagForegroundDeletes(t *testing.T) {
	del, err := resolveRmFlag(true, false)
	if err != nil {
		t.Fatalf("resolveRmFlag(true, false) = %v, want nil error", err)
	}
	if !del {
		t.Error("autoDelete = false, want true — --rm triggers delete-on-exit")
	}
}

func TestResolveRmFlagNoRmPersists(t *testing.T) {
	del, err := resolveRmFlag(false, false)
	if err != nil || del {
		t.Errorf("resolveRmFlag(false, false) = (%v, %v), want (false, nil)", del, err)
	}
}

func TestResolveRunNameUsesFlagName(t *testing.T) {
	if got := resolveRunName("mybox", map[string]bool{}); got != "mybox" {
		t.Errorf("resolveRunName = %q, want mybox", got)
	}
}

func TestResolveRunNameGeneratesWhenEmpty(t *testing.T) {
	if got := resolveRunName("", map[string]bool{}); got == "" {
		t.Error("resolveRunName(\"\") = empty, want a generated name")
	}
}

func TestNewRunCmdFlagsAlignToContract(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newRunCmd(store)
	for _, f := range []struct{ long, short string }{
		{"cpus", "c"}, {"memory", "m"}, {"volume", "v"}, {"publish", "p"},
		{"env", "e"}, {"detach", "d"}, {"interactive", "i"}, {"tty", "t"},
		{"workdir", "w"}, {"user", "u"},
	} {
		fl := cmd.Flags().Lookup(f.long)
		if fl == nil {
			t.Errorf("--%s not registered", f.long)
			continue
		}
		if fl.Shorthand != f.short {
			t.Errorf("--%s shorthand = %q, want %q", f.long, fl.Shorthand, f.short)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestResolveRmFlag|TestResolveRunName|TestNewRunCmd' -v`
Expected: FAIL (compile / semantics).

- [ ] **Step 3: Rewrite the helpers + `runRun` in `internal/cli/run.go`**

```go
// resolveRmFlag validates --rm against --detach and reports whether the VM
// should be auto-deleted when its foreground command exits. run now persists
// by default (container semantics); --rm opts into ephemeral cleanup. Detached
// VMs can't be reaped, so --rm -d is an error.
func resolveRmFlag(rm, detach bool) (autoDelete bool, err error) {
	if rm && detach {
		return false, fmt.Errorf("--rm requires a foreground command; detached VMs can't be reaped on exit yet (clean up with: mvm delete <name>)")
	}
	return rm, nil
}

// resolveRunName decides the VM's name: an explicit --name is used verbatim,
// else a fresh adjective-noun name is generated. Durability is no longer tied
// to the name — run persists by default; see resolveRmFlag.
func resolveRunName(nameFlag string, existing map[string]bool) string {
	if nameFlag != "" {
		return nameFlag
	}
	return GenerateVMName(existing)
}

func runRun(store *state.Store, image string, cmdArgs []string, nameFlag string, detach bool, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, interactive bool, workdir string, envVars []string, user string, rm bool) error {
	autoDelete, err := resolveRmFlag(rm, detach)
	if err != nil {
		return err
	}
	resolvedImage := resolveImage(image)
	existing, err := existingVMNames(store)
	if err != nil {
		return err
	}
	name := resolveRunName(nameFlag, existing)

	if err := runStart(store, name, true, ports, netPolicy, volumes, "", "", cpus, memoryMB, resolvedImage, false, nil, nil, true); err != nil {
		return fmt.Errorf("start %q: %w", name, err)
	}

	cleanup := func() {
		if autoDelete {
			if err := runDelete(store, name, true); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to clean up VM %q: %v\n", name, err)
			}
		}
	}

	if detach {
		fmt.Printf("%s\n", name)
		return nil
	}

	if err := waitForReady(30*time.Second, func() error {
		return runExec(store, name, []string{"true"}, false, false, "", nil, "")
	}); err != nil {
		cleanup()
		return fmt.Errorf("VM %q never became ready: %w", name, err)
	}

	if len(cmdArgs) == 0 {
		cmdArgs = []string{"/bin/bash"}
		interactive = true
	}
	execErr := runExec(store, name, cmdArgs, interactive, false, workdir, envVars, user)
	cleanup()
	return execErr
}
```

- [ ] **Step 4: Update `newRunCmd`** — Short/Long text and the changed flag registrations:

```go
		Short: "Boot a VM from an image and run a command; the VM persists by default",
		Long: `Boot a VM from an image, image-first (Docker-style).

The VM persists after the command exits (container run semantics). Pass --rm
to auto-delete it on exit. Without --name the VM is auto-named. "base" is the
default rootfs.

  mvm run base -- ls /                  # boots, runs, PERSISTS
  mvm run base --rm -- ls /             # boots, runs, deletes
  mvm run base --name mybox -- bash     # persists as "mybox"
  mvm run base -d                       # boot and detach, no command
  mvm run base -p 8080:80 -- serve`,
```

```go
	cmd.Flags().IntVarP(&cpus, "cpus", "c", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVarP(&memoryMB, "memory", "m", 0, "RAM in MiB (default: 1024)")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "v", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().BoolVar(&rm, "rm", false, "auto-delete the VM when the foreground command exits (error with -d)")
```

- [ ] **Step 5: Verify**

Run: `go test ./internal/cli/ -run 'TestResolveRmFlag|TestResolveRunName|TestNewRunCmd|TestRunRun' -v && go build ./...`
Expected: PASS (existing `TestRunRun*` still pass — call sites pass `rm=false`), clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/run.go internal/cli/run_test.go
git commit -m "feat(cli): run persists by default; --rm auto-deletes; align flags (-c/-m/-v)"
```

---

### Task 10: `list` — container-shaped JSON + running-only default (source B2)

**Files:**
- Modify: `internal/cli/list.go`
- Test: `internal/cli/list_test.go`

**Interfaces:**
- Produces: `func runList(store *state.Store, jsonOutput, quiet, all bool) error` (gains `all`); `func filterRunning(vms []server.VMResponse, all bool) []server.VMResponse`; `func specsByName(store *state.Store) map[string]*state.VMSpec` (consumed by Task 12).
- Consumes: `toCFContainers` (**Task 2**), `localApplevzVMs`, `mergeVMResponses`, `requireDaemon`, `resolveFormat`, `timeAgo`.

- [ ] **Step 1: Write the failing test** — `internal/cli/list_test.go`:

```go
func TestFilterRunning(t *testing.T) {
	vms := []server.VMResponse{
		{Name: "a", Status: "running"},
		{Name: "b", Status: "stopped"},
		{Name: "c", Status: "paused"},
	}
	if got := filterRunning(vms, false); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("default should keep only running, got %+v", got)
	}
	if got := filterRunning(vms, true); len(got) != 3 {
		t.Fatalf("--all should keep all, got %d", len(got))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run TestFilterRunning -v`
Expected: FAIL — `filterRunning` undefined.

- [ ] **Step 3: Edit `newListCmd`** to add `-a/--all` and thread it:

```go
func newListCmd(store *state.Store) *cobra.Command {
	var (
		jsonOutput bool
		quiet      bool
		all        bool
		format     string
	)
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List microVMs (running only by default; -a for all)",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := resolveFormat(format, jsonOutput)
			if err != nil {
				return err
			}
			return runList(store, wantJSON, quiet, all)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON (alias for --format json)")
	cmd.Flags().StringVar(&format, "format", "", "output format: table (default) or json")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "only print VM names")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped VMs (default: running only)")
	return cmd
}
```

- [ ] **Step 4: Rewrite `runList`** — filter, then emit container JSON:

```go
func runList(store *state.Store, jsonOutput, quiet, all bool) error {
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	var daemonVMs []server.VMResponse
	if sc, err := requireDaemon(); err == nil {
		if vms, err := sc.ListVMs(context.Background()); err == nil {
			daemonVMs = vms
		}
	}
	vms := filterRunning(mergeVMResponses(localVMs, daemonVMs), all)
	sort.Slice(vms, func(i, j int) bool { return vms[i].CreatedAt.Before(vms[j].CreatedAt) })

	if jsonOutput {
		data, _ := json.MarshalIndent(toCFContainers(vms, specsByName(store)), "", "  ")
		fmt.Println(string(data))
		return nil
	}
	if len(vms) == 0 {
		if !quiet {
			fmt.Println("No microVMs. Create one with: mvm create <name>")
		}
		return nil
	}
	if quiet {
		for _, vm := range vms {
			fmt.Println(vm.Name)
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tIP\tBACKEND\tCREATED")
	for _, vm := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", vm.Name, vm.Status, vm.GuestIP, vm.Backend, timeAgo(vm.CreatedAt))
	}
	w.Flush()
	return nil
}

// filterRunning drops stopped/paused VMs unless all is set (container default).
func filterRunning(vms []server.VMResponse, all bool) []server.VMResponse {
	if all {
		return vms
	}
	out := make([]server.VMResponse, 0, len(vms))
	for _, vm := range vms {
		if vm.Status == "running" {
			out = append(out, vm)
		}
	}
	return out
}

// specsByName maps VM name → persisted spec, so container-shaped JSON can
// populate image/cpus/memory that VMResponse omits.
func specsByName(store *state.Store) map[string]*state.VMSpec {
	m := map[string]*state.VMSpec{}
	all, err := store.ListVMs()
	if err != nil {
		return m
	}
	for _, vm := range all {
		if vm.Spec != nil {
			m[vm.Name] = vm.Spec
			continue
		}
		m[vm.Name] = &state.VMSpec{Cpus: vm.Cpus, MemoryMB: vm.MemoryMB}
	}
	return m
}
```

- [ ] **Step 5: Verify + integration script**

Run: `go test ./internal/cli/ -run 'TestFilterRunning|TestToCF' -v && go build ./...`
Update `list_test.go` call sites for the new `all` arg. Update `scripts/integration-test.sh` where it expects a stopped VM in `list` output (add `-a`).
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/list.go internal/cli/list_test.go scripts/integration-test.sh
git commit -m "feat(cli): list running-only by default (-a/--all) + container-shaped JSON"
```

---

### Task 11: `inspect` — 1-element array with platform (source B3)

**Files:**
- Modify: `internal/cli/inspect.go`
- Test: `internal/cli/inspect_test.go`

**Interfaces:** `printInspect(resp server.VMInspectResponse) error` now emits `[]cfContainer` (inspect=true). Table path unchanged. Consumes `toCFContainer` (**Task 2**).

- [ ] **Step 1: Write the failing test** — `internal/cli/inspect_test.go`:

```go
func TestInspectJSONIsOneElementArrayWithPlatform(t *testing.T) {
	resp := server.VMInspectResponse{
		VMResponse: server.VMResponse{
			Name: "web", Status: "running", GuestIP: "192.168.64.5",
			CreatedAt: time.Unix(1700000000, 0),
		},
		Spec: &state.VMSpec{Image: "nginx", Cpus: 2, MemoryMB: 512},
	}
	arr := []cfContainer{toCFContainer(resp.VMResponse, resp.Spec, true)}
	if len(arr) != 1 {
		t.Fatalf("inspect must wrap in a 1-element array, got %d", len(arr))
	}
	p := arr[0].Configuration.Platform
	if p == nil || p.OS != "linux" || p.Architecture != "arm64" {
		t.Fatalf("platform must be linux/arm64, got %+v", p)
	}
	if arr[0].Configuration.Resources.MemoryInBytes != 512*1024*1024 {
		t.Fatalf("memoryInBytes nesting wrong: %d", arr[0].Configuration.Resources.MemoryInBytes)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run TestInspect -v`
Expected: PASS-compiles-fails-if-shape-wrong — actually this test drives the transform directly, so it validates the shape; if `printInspect` isn't updated it still passes at the transform level. Proceed to Step 3 to wire the command output.

- [ ] **Step 3: Edit `printInspect`** (JSON default) to wrap:

```go
func printInspect(resp server.VMInspectResponse) error {
	// container's inspect returns a JSON array the client reads [0] from.
	arr := []cfContainer{toCFContainer(resp.VMResponse, resp.Spec, true)}
	data, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
```

`printInspectTable` and `runInspect` are unchanged.

- [ ] **Step 4: Verify**

Run: `go test ./internal/cli/ -run TestInspect -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/inspect.go internal/cli/inspect_test.go
git commit -m "feat(cli): inspect emits container 1-element array with platform"
```

---

### Task 12: `stats` — container `cfStats` + cumulative CPU µs (source B4)

> **Note:** this pulls cumulative-CPU fidelity forward from Slice 3 because `cfStats.cpuUsageUsec` is meaningless as an instantaneous value. applevz gets true cumulative µs now; the Firecracker daemon path (frozen `VMStats`) reports memory + process count with `cpuUsageUsec=0` until an additive daemon stats endpoint lands (Slice 3).

**Files:**
- Modify: `internal/firecracker/stats.go` (add `ParseCumulativePS`, `parseCPUTime`)
- Test: `internal/firecracker/stats_test.go`
- Modify: `internal/cli/stats.go`
- Test: `internal/cli/stats_test.go`

**Interfaces:**
- Produces: `func ParseCumulativePS(out string) (cpuUsec uint64, memMB float64, err error)`; `func parseCPUTime(s string) (uint64, error)`; `func hostCumulativeStats(pid int) (uint64, float64, error)`; `func memLimitBytes(spec *state.VMSpec) uint64`; `func filterSourcesByName(all []cfStatSource, names []string) []cfStatSource`. `runStats` keeps its signature. Replaces `hostProcessStats`/`filterStatsByName`.
- Consumes: `toCFStats`/`cfStatSource` (**Task 2**), `specsByName` (**Task 10**).

- [ ] **Step 1: Parse tests first** — `internal/firecracker/stats_test.go`:

```go
func TestParseCPUTime(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"00:12.50", 12_500_000},
		{"01:02", 62_000_000},
		{"01:02:03", 3_723_000_000},
		{"1-00:00:00", 86_400_000_000},
	}
	for _, c := range cases {
		got, err := parseCPUTime(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q: got %d want %d", c.in, got, c.want)
		}
	}
}

func TestParseCumulativePS(t *testing.T) {
	cpu, memMB, err := ParseCumulativePS("  00:12.50 102400\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cpu != 12_500_000 {
		t.Fatalf("cpu: got %d want 12500000", cpu)
	}
	if memMB != 100.0 {
		t.Fatalf("mem: got %v want 100", memMB)
	}
}

func TestParseCumulativePSBadFields(t *testing.T) {
	if _, _, err := ParseCumulativePS("only-one-field"); err == nil {
		t.Fatal("want error on malformed ps output")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/firecracker/ -run 'ParseCPUTime|ParseCumulativePS' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement in `internal/firecracker/stats.go`** (append; `ParsePSOutput`/`ProcessStats` untouched):

```go
// ParseCumulativePS parses `ps -o time=,rss= -p <pid>` — two fields: cumulative
// CPU time [[DD-]HH:]MM:SS[.ff] and resident memory in KiB. Returns cumulative
// CPU in microseconds (monotonic) and memory in MiB. Portable across BSD ps
// (macOS/applevz) and Linux ps (Lima/firecracker) — both spell cumulative CPU
// as the `time` keyword.
func ParseCumulativePS(out string) (cpuUsec uint64, memMB float64, err error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ps output %q (want 2 fields: time rss)", out)
	}
	cpuUsec, err = parseCPUTime(fields[0])
	if err != nil {
		return 0, 0, err
	}
	rssKB, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse rss %q: %w", fields[1], err)
	}
	return cpuUsec, rssKB / 1024.0, nil
}

// parseCPUTime converts a ps `time` field — [[DD-]HH:]MM:SS[.ff] — to microseconds.
func parseCPUTime(s string) (uint64, error) {
	days := 0
	rest := s
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		d, err := strconv.Atoi(rest[:i])
		if err != nil {
			return 0, fmt.Errorf("parse cpu days %q: %w", s, err)
		}
		days = d
		rest = rest[i+1:]
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("parse cpu time %q: want [[DD-]HH:]MM:SS", s)
	}
	var hours, mins int
	var secs float64
	var err error
	if len(parts) == 3 {
		if hours, err = strconv.Atoi(parts[0]); err != nil {
			return 0, fmt.Errorf("parse cpu hours %q: %w", s, err)
		}
		parts = parts[1:]
	}
	if mins, err = strconv.Atoi(parts[0]); err != nil {
		return 0, fmt.Errorf("parse cpu minutes %q: %w", s, err)
	}
	if secs, err = strconv.ParseFloat(parts[1], 64); err != nil {
		return 0, fmt.Errorf("parse cpu seconds %q: %w", s, err)
	}
	total := float64(days)*86400 + float64(hours)*3600 + float64(mins)*60 + secs
	return uint64(total * 1e6), nil
}
```

Run: `go test ./internal/firecracker/ -run 'ParseCPUTime|ParseCumulativePS' -v` → PASS.

- [ ] **Step 4: Rewrite `runStats`** in `internal/cli/stats.go`:

```go
func runStats(store *state.Store, names []string, wantJSON bool) error {
	specs := specsByName(store)
	sources := []cfStatSource{}

	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	for _, vm := range localVMs {
		src := cfStatSource{
			Name: vm.Name, Backend: vm.Backend, PID: vm.PID, Status: vm.Status,
			MemoryLimitBytes: memLimitBytes(specs[vm.Name]), NumProcesses: 1,
		}
		if vm.Status == "running" && vm.PID > 0 {
			if cpuUsec, memMB, err := hostCumulativeStats(vm.PID); err == nil {
				src.CPUUsageUsec = cpuUsec
				src.MemoryUsageBytes = uint64(memMB * 1024 * 1024)
			}
		}
		sources = append(sources, src)
	}

	if sc, err := requireDaemon(); err == nil {
		if stats, err := sc.StatsVMs(context.Background()); err == nil {
			for _, s := range stats {
				sources = append(sources, cfStatSource{
					Name: s.Name, Backend: s.Backend, PID: s.PID, Status: s.Status,
					MemoryUsageBytes: uint64(s.MemMB * 1024 * 1024),
					MemoryLimitBytes: memLimitBytes(specs[s.Name]), NumProcesses: 1,
				})
			}
		}
	}

	sources = filterSourcesByName(sources, names)

	if wantJSON {
		data, _ := json.MarshalIndent(toCFStats(sources), "", "  ")
		fmt.Println(string(data))
		return nil
	}
	if len(sources) == 0 {
		fmt.Println("No microVMs.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tBACKEND\tPID\tCPU(s)\tMEM(MiB)\tPROCS\tSTATUS")
	for _, s := range sources {
		cpu, mem := "-", "-"
		if s.Status == "running" {
			cpu = fmt.Sprintf("%.1f", float64(s.CPUUsageUsec)/1e6)
			mem = fmt.Sprintf("%.0f", float64(s.MemoryUsageBytes)/(1024*1024))
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%d\t%s\n", s.Name, s.Backend, s.PID, cpu, mem, s.NumProcesses, s.Status)
	}
	return w.Flush()
}

func hostCumulativeStats(pid int) (cpuUsec uint64, memMB float64, err error) {
	out, err := exec.Command("ps", "-o", "time=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return firecracker.ParseCumulativePS(string(out))
}

func memLimitBytes(spec *state.VMSpec) uint64 {
	if spec == nil {
		return 0
	}
	return uint64(spec.MemoryMB) * 1024 * 1024
}

func filterSourcesByName(all []cfStatSource, names []string) []cfStatSource {
	if len(names) == 0 {
		return all
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	filtered := []cfStatSource{}
	for _, s := range all {
		if want[s.Name] {
			filtered = append(filtered, s)
		}
	}
	return filtered
}
```

Delete `hostProcessStats` and `filterStatsByName`. Drop the `server` import from `stats.go` if now unused (keep `firecracker`, `state`, `exec`, `strconv`). `--no-stream` flag unchanged.

- [ ] **Step 5: Stats JSON-shape test** — `internal/cli/stats_test.go`:

```go
func TestStatsJSONShape(t *testing.T) {
	src := []cfStatSource{
		{Name: "web", CPUUsageUsec: 12_500_000, MemoryUsageBytes: 104857600, MemoryLimitBytes: 536870912, NumProcesses: 1},
		{Name: "db", MemoryLimitBytes: 268435456, NumProcesses: 1},
	}
	got := filterSourcesByName(src, []string{"web"})
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("name filter: got %+v", got)
	}
	out, _ := json.MarshalIndent(toCFStats(got), "", "  ")
	want := `[
  {
    "id": "web",
    "cpuUsageUsec": 12500000,
    "memoryUsageBytes": 104857600,
    "memoryLimitBytes": 536870912,
    "numProcesses": 1
  }
]`
	if string(out) != want {
		t.Fatalf("stats json:\n got:\n%s\nwant:\n%s", out, want)
	}
}
```

- [ ] **Step 6: Verify + commit**

Run: `go test ./internal/firecracker/ ./internal/cli/ -run 'ParseCPU|ParseCumulative|Stats' -v && go vet ./...`
Expected: PASS.

```bash
git add internal/firecracker/stats.go internal/firecracker/stats_test.go internal/cli/stats.go internal/cli/stats_test.go
git commit -m "feat(cli): stats emits container cfStats with cumulative CPU microseconds"
```

---

### Task 13: `exec` — no `--` separator (source B5)

**Files:**
- Modify: `internal/cli/exec.go`
- Test: `internal/cli/exec_test.go`

**Interfaces:** `newExecCmd` unchanged signature; adds `cmd.Flags().SetInterspersed(false)`. `args[0]`=name, `args[1:]`=command. `MinimumNArgs(2)` stays.

- [ ] **Step 1: Write the failing tests** — `internal/cli/exec_test.go`:

```go
func TestExecNoSeparatorTakesCommandDirectly(t *testing.T) {
	cmd := newExecCmd(nil)
	var got []string
	cmd.RunE = func(c *cobra.Command, args []string) error { got = args; return nil }
	cmd.SetArgs([]string{"web", "ls", "-la"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"web", "ls", "-la"}
	if len(got) != len(want) {
		t.Fatalf("args: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestExecLeadingFlagStillBinds(t *testing.T) {
	cmd := newExecCmd(nil)
	var got []string
	cmd.RunE = func(c *cobra.Command, args []string) error { got = args; return nil }
	cmd.SetArgs([]string{"-i", "web", "env"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(got) != 2 || got[0] != "web" || got[1] != "env" {
		t.Fatalf("args: got %v want [web env]", got)
	}
}
```

(Add `"io"` to the test imports.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run TestExec -v`
Expected: FAIL — without `--`, cobra treats `-la` as an unknown exec flag.

- [ ] **Step 3: Edit `newExecCmd`** — add `SetInterspersed(false)` and drop `--` from `Use`/`Long`:

```go
	cmd := &cobra.Command{
		Use:   "exec <name> <command> [args...]",
		Short: "Run a command in a running microVM",
		Long: `Run a command inside a running microVM.

  mvm exec my-vm ls /
  mvm exec my-vm -it bash
  mvm exec my-vm -e FOO=bar env
  mvm exec my-vm -d long-running-task
  echo "data" | mvm exec my-vm cat`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateExecFlags(detach, interactive, tty); err != nil {
				return err
			}
			name := args[0]
			remoteArgs := args[1:]
			allEnv, err := mergeEnvFile(envFile, envVars)
			if err != nil {
				return err
			}
			return runExec(store, name, remoteArgs, interactive || tty, detach, workdir, allEnv, user)
		},
	}
	// Stop flag parsing at the first positional (<name>) so everything after is
	// taken verbatim as the guest command — no `--` separator required.
	cmd.Flags().SetInterspersed(false)
```

- [ ] **Step 4: Verify + update callers**

Run: `go test ./internal/cli/ -run TestExec -v && go build ./...`
Then: `rg -n 'exec .* -- ' scripts/ docs/` and drop the `--` from any hits (notably `scripts/integration-test.sh`).
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/exec.go internal/cli/exec_test.go scripts/integration-test.sh
git commit -m "feat(cli): exec takes the command directly, no -- separator"
```

---

### Task 14: `image` noun — rename + `ls`/`rm` + `--format json` (source C1)

**Files:**
- Delete `internal/cli/images.go`; Create `internal/cli/image.go`, `internal/cli/image_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Produces: `func newImageCmd(store *state.Store) *cobra.Command` (`image`, alias `i`; `ls`/`rm`); `func imagesToCF(imgs []server.ImageInfo) []cfImage`. (The `store` param is added now so Task 16's `prune` can reuse the same constructor.)
- Consumes: `cfImage`/`cfDescriptor` (**Task 2**), `requireDaemon`, `Client.ImageList/ImageDelete`, `server.ImageInfo{Name, SizeMB}`.

- [ ] **Step 1: Write the failing tests** — `internal/cli/image_test.go`:

```go
package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

func TestImagesToCF(t *testing.T) {
	got := imagesToCF([]server.ImageInfo{{Name: "web", SizeMB: 128}, {Name: "db", SizeMB: 0}})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Reference != "web" || got[0].Descriptor.Size != 128*1024*1024 || got[0].Descriptor.Digest != "" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Descriptor.Size != 0 {
		t.Errorf("zero-MiB size = %d, want 0", got[1].Descriptor.Size)
	}
}

func TestImagesToCFEmptyMarshalsToArray(t *testing.T) {
	b, _ := json.Marshal(imagesToCF(nil))
	if string(b) != "[]" {
		t.Errorf("marshal(nil) = %s, want []", b)
	}
}

func TestImageCmdWiring(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	c := newImageCmd(store)
	if c.Use != "image" {
		t.Fatalf("Use = %q, want image", c.Use)
	}
	names := map[string]bool{}
	for _, sub := range c.Commands() {
		names[sub.Name()] = true
	}
	if !names["ls"] || !names["rm"] {
		t.Fatalf("subcommands = %v, want ls+rm", names)
	}
	ls, _, err := c.Find([]string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	if ls.Flags().Lookup("format") == nil {
		t.Error("ls missing --format")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestImagesToCF|TestImageCmd' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Create `internal/cli/image.go`** (delete `images.go`):

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

func newImageCmd(store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image",
		Short:   "Manage custom rootfs images",
		Aliases: []string{"i"},
	}
	cmd.AddCommand(newImageLsCmd(), newImageRmCmd())
	return cmd
}

func newImageLsCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List custom rootfs images",
		Aliases: []string{"list"},
		RunE:    func(cmd *cobra.Command, args []string) error { return runImageLs(cmd.Context(), format) },
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}

func newImageRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a custom rootfs image",
		Aliases: []string{"delete"},
		Args:    cobra.ExactArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return runImageRm(cmd.Context(), args[0]) },
	}
}

// imagesToCF transforms ImageInfo (MiB) into cfImage (bytes). Pure; digest is
// empty until the OCI image store lands (Slice 3).
func imagesToCF(imgs []server.ImageInfo) []cfImage {
	out := make([]cfImage, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, cfImage{Reference: img.Name, Descriptor: cfDescriptor{Size: int64(img.SizeMB) * 1024 * 1024}})
	}
	return out
}

func runImageLs(ctx context.Context, format string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	images, err := sc.ImageList(ctx)
	if err != nil {
		return err
	}
	if format == "json" {
		data, err := json.MarshalIndent(imagesToCF(images), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	if len(images) == 0 {
		fmt.Println("No custom images. Build one with: mvm build -f Dockerfile -t <name>")
		return nil
	}
	fmt.Printf("%-20s %s\n", "REFERENCE", "SIZE (MiB)")
	for _, img := range images {
		fmt.Printf("%-20s %d\n", img.Name, img.SizeMB)
	}
	return nil
}

func runImageRm(ctx context.Context, name string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	if err := sc.ImageDelete(ctx, name); err != nil {
		return err
	}
	fmt.Printf("  Image '%s' removed\n", name)
	return nil
}
```

- [ ] **Step 4: Register** in `internal/cli/root.go` — replace `newImagesCmd(),` with `newImageCmd(store),`.

- [ ] **Step 5: Verify + commit**

Run: `go test ./internal/cli/ -run 'TestImagesToCF|TestImageCmd' -v && go build ./...`

```bash
git add internal/cli/image.go internal/cli/image_test.go internal/cli/root.go
git rm internal/cli/images.go
git commit -m "feat(cli): rename images noun to image with ls/rm and --format json"
```

---

### Task 15: `image inspect <name>` (source C2)

**Files:** Modify `internal/cli/image.go`, `internal/cli/image_test.go`.

**Interfaces:** `newImageInspectCmd()`; `imageToCF(img server.ImageInfo) cfImage`; `findImage(imgs, name) (server.ImageInfo, error)`. Refactor `imagesToCF` to call `imageToCF`.

- [ ] **Step 1: Test** — append to `internal/cli/image_test.go`:

```go
func TestFindImage(t *testing.T) {
	imgs := []server.ImageInfo{{Name: "web", SizeMB: 64}}
	got, err := findImage(imgs, "web")
	if err != nil {
		t.Fatal(err)
	}
	cf := imageToCF(got)
	if cf.Reference != "web" || cf.Descriptor.Size != 64*1024*1024 {
		t.Errorf("cf = %+v", cf)
	}
	if _, err := findImage(imgs, "missing"); err == nil {
		t.Error("expected error for missing image")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run TestFindImage -v` → FAIL.

- [ ] **Step 3: Add to `internal/cli/image.go`:**

```go
func newImageInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show detailed information for one custom rootfs image (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runImageInspect(cmd.Context(), args[0]) },
	}
}

func imageToCF(img server.ImageInfo) cfImage {
	return cfImage{Reference: img.Name, Descriptor: cfDescriptor{Size: int64(img.SizeMB) * 1024 * 1024}}
}

func findImage(imgs []server.ImageInfo, name string) (server.ImageInfo, error) {
	for _, img := range imgs {
		if img.Name == name {
			return img, nil
		}
	}
	return server.ImageInfo{}, fmt.Errorf("image %q not found", name)
}

func runImageInspect(ctx context.Context, name string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	images, err := sc.ImageList(ctx)
	if err != nil {
		return err
	}
	img, err := findImage(images, name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(imageToCF(img), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
```

Add `newImageInspectCmd(),` to `newImageCmd`'s `AddCommand`; refactor `imagesToCF` loop body to `out = append(out, imageToCF(img))`.

- [ ] **Step 4: Verify + commit**

Run: `go test ./internal/cli/ -run 'TestFindImage|TestImage' -v && go build ./...`

```bash
git add internal/cli/image.go internal/cli/image_test.go
git commit -m "feat(cli): add image inspect subcommand"
```

---

### Task 16: `image prune` — remove unreferenced images (source C3)

**Files:** Modify `internal/cli/image.go`, `internal/cli/image_test.go`.

**Interfaces:** `newImagePruneCmd(store *state.Store)`; `unreferencedImages(imgs []server.ImageInfo, vms []*state.VM) []string`. `newImageCmd` already takes `store` (Task 14) — no signature change.

- [ ] **Step 1: Test** — append:

```go
func TestUnreferencedImages(t *testing.T) {
	imgs := []server.ImageInfo{{Name: "used"}, {Name: "orphan"}}
	vms := []*state.VM{
		{Name: "vm1", Spec: &state.VMSpec{Image: "used"}},
		{Name: "vm2", Spec: nil},
	}
	got := unreferencedImages(imgs, vms)
	if len(got) != 1 || got[0] != "orphan" {
		t.Fatalf("unreferenced = %v, want [orphan]", got)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run TestUnreferencedImages -v` → FAIL.

- [ ] **Step 3: Add to `internal/cli/image.go`:**

```go
func newImagePruneCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Remove custom rootfs images not referenced by any VM",
		RunE:  func(cmd *cobra.Command, args []string) error { return runImagePrune(cmd.Context(), store) },
	}
}

func unreferencedImages(imgs []server.ImageInfo, vms []*state.VM) []string {
	inUse := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if vm.Spec != nil && vm.Spec.Image != "" {
			inUse[vm.Spec.Image] = true
		}
	}
	var out []string
	for _, img := range imgs {
		if !inUse[img.Name] {
			out = append(out, img.Name)
		}
	}
	return out
}

func runImagePrune(ctx context.Context, store *state.Store) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	images, err := sc.ImageList(ctx)
	if err != nil {
		return err
	}
	vms, err := store.ListVMs()
	if err != nil {
		return err
	}
	unused := unreferencedImages(images, vms)
	if len(unused) == 0 {
		fmt.Println("No unused images to prune.")
		return nil
	}
	for _, name := range unused {
		if err := sc.ImageDelete(ctx, name); err != nil {
			return fmt.Errorf("remove %q: %w", name, err)
		}
		fmt.Printf("  Removed image '%s'\n", name)
	}
	return nil
}
```

Add `newImagePruneCmd(store),` to `newImageCmd`'s `AddCommand`.

- [ ] **Step 4: Verify + commit**

Run: `go test ./internal/cli/ -run 'TestUnreferencedImages|TestImage' -v && go build ./...`

```bash
git add internal/cli/image.go internal/cli/image_test.go
git commit -m "feat(cli): add image prune for unreferenced rootfs images"
```

---

### Task 17: `system` noun scaffold — absorb serve/doctor/version (source C4)

**Breaking, in place.** Move logic; do not duplicate.

**Files:**
- Create: `internal/cli/system.go`, `internal/cli/system_test.go`
- Modify: `internal/cli/root.go` (register `system`; remove top-level `serve`/`doctor`/`version`), `internal/cli/serve.go` (extract `runServeStopE`)

**Interfaces:**
- Produces: `newSystemCmd(limaClient *lima.Client, store *state.Store, version, commit, date string) *cobra.Command` with subcommands `status` (Task 18), `df` (Task 19), `version`, `logs`, `start`, `stop`; `newSystemVersionCmd`, `newSystemStartCmd`, `newSystemStopCmd`, `newSystemLogsCmd`; `runServeStopE(cmd, args) error` (extracted).
- `system version`'s RunE writes via `cmd.OutOrStdout()`.

- [ ] **Step 1: Write the failing tests** — `internal/cli/system_test.go`:

```go
package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func testSystemCmd(t *testing.T) *cobra.Command {
	t.Helper()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	return newSystemCmd(lima.NewClient("mvm"), store, "1.2.3", "abc123", "2026-07-21")
}

func TestSystemSubcommandsRegistered(t *testing.T) {
	c := testSystemCmd(t)
	if c.Use != "system" {
		t.Fatalf("Use = %q, want system", c.Use)
	}
	have := map[string]bool{}
	for _, sub := range c.Commands() {
		have[sub.Name()] = true
	}
	// Task 17 wires these four; status/df are added by Tasks 18/19 (which
	// extend this test to assert them).
	for _, w := range []string{"version", "logs", "start", "stop"} {
		if !have[w] {
			t.Errorf("missing subcommand %q (have %v)", w, have)
		}
	}
}

func TestSystemVersionPrints(t *testing.T) {
	c := testSystemCmd(t)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{"version"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "1.2.3") {
		t.Errorf("output %q missing version 1.2.3", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run TestSystem -v` → FAIL.

- [ ] **Step 3: Create `internal/cli/system.go`** with the parent + `version`/`start`/`stop`/`logs`. `status` (Task 18) and `df` (Task 19) are added to `newSystemCmd`'s `AddCommand` when those tasks land, so this task compiles on its own:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newSystemCmd(limaClient *lima.Client, store *state.Store, version, commit, date string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "system",
		Short:   "Inspect and manage the mvm system and daemon",
		Aliases: []string{"s"},
	}
	// status (Task 18) and df (Task 19) are added to this AddCommand when those
	// tasks land — keeping each task independently compilable. Task 17 wires
	// only the four it defines.
	cmd.AddCommand(
		newSystemVersionCmd(version, commit, date),
		newSystemLogsCmd(),
		newSystemStartCmd(limaClient, store),
		newSystemStopCmd(),
	)
	return cmd
}

func newSystemVersionCmd(version, commit, date string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print mvm version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "mvm %s (commit: %s, built: %s)\n", version, commit, date)
		},
	}
}

func newSystemStartCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	var socketPath, listenAddr, tlsCert, tlsKey, apiKeyFlag, apiKeyFile string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the mvm daemon (foreground)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeStart(limaClient, store, socketPath, listenAddr, tlsCert, tlsKey, apiKeyFlag, apiKeyFile)
		},
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (default: ~/.mvm/server.sock)")
	cmd.Flags().StringVar(&listenAddr, "listen", "", "TCP listen address (e.g. 0.0.0.0:19876)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "TLS certificate file")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "TLS private key file")
	cmd.Flags().StringVar(&apiKeyFlag, "api-key", "", "API key for TCP auth")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "File containing API key")
	return cmd
}

func newSystemStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Stop the mvm daemon", RunE: runServeStopE}
}

func newSystemLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the mvm daemon log",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, _ := os.UserHomeDir()
			return tailFile(cmd.OutOrStdout(), filepath.Join(home, ".mvm", "serve.log"), follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the log")
	return cmd
}
```

Extract `runServeStopE(cmd *cobra.Command, args []string) error` in `serve.go` from the existing stop RunE (send SIGTERM to the daemon PID; write via `cmd.OutOrStdout()`), and add a minimal `tailFile(w io.Writer, path string, follow bool) error` helper in `system.go` (read + print; `follow` polls appended bytes). Keep `runServeStart`, `installServeLaunchd`, etc. that `install`/launchd still call; delete `newServeCmd`/`newVersionCmd`/`newDoctorCmd`'s top-level constructors only if fully orphaned after Step 4 (verify with `go build`).

- [ ] **Step 4: Edit `internal/cli/root.go`** — remove `newVersionCmd(...)`, `newDoctorCmd(...)`, `newServeCmd(...)` from `AddCommand`; add `newSystemCmd(limaClient, store, version, commit, date),`.

- [ ] **Step 5: Verify** — `go build ./...` (resolve orphaned funcs), `go vet ./...`, `go test ./internal/cli/ -run TestSystem -v`. Confirm `mvm version|doctor|serve` are gone and `mvm system version` works. Update `scripts/integration-test.sh` for any removed top-level verb.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/system.go internal/cli/system_test.go internal/cli/root.go internal/cli/serve.go scripts/integration-test.sh
git commit -m "feat(cli)!: introduce system noun; move serve/doctor/version under it (breaking)"
```

---

### Task 18: `system status` — backend-aware, folds in doctor (source C5)

**Files:** Modify `internal/cli/system.go`, `internal/cli/system_test.go`.

**Interfaces:** `newSystemStatusCmd(limaClient *lima.Client, store *state.Store, version string)` (`--format json|table`); `type systemStatus`; `buildSystemStatus(backend string, daemonUp bool, socket, version string) systemStatus`; `renderSystemStatusText(s systemStatus) string`. `renderSystemStatusText` emits the load-bearing substrings `"is running"`, `"container-apiserver version: "`, `"application install root: "`; applevz emits `"applevz backend — no daemon required"`.

- [ ] **Step 1: Write the failing tests** — append to `internal/cli/system_test.go` (`import "encoding/json"`):

```go
func TestBuildSystemStatusFirecrackerRunning(t *testing.T) {
	txt := renderSystemStatusText(buildSystemStatus("firecracker", true, "/root/.mvm/server.sock", "1.2.3"))
	for _, sub := range []string{"is running", "container-apiserver version: ", "application install root: "} {
		if !strings.Contains(txt, sub) {
			t.Errorf("text missing %q:\n%s", sub, txt)
		}
	}
}

func TestBuildSystemStatusFirecrackerStopped(t *testing.T) {
	s := buildSystemStatus("firecracker", false, "", "1.2.3")
	if s.DaemonRunning {
		t.Error("DaemonRunning = true, want false")
	}
	if strings.Contains(renderSystemStatusText(s), "is running") {
		t.Error("stopped daemon should not report \"is running\"")
	}
}

func TestBuildSystemStatusApplevz(t *testing.T) {
	txt := renderSystemStatusText(buildSystemStatus("applevz", false, "", "1.2.3"))
	if !strings.Contains(txt, "applevz backend — no daemon required") {
		t.Errorf("applevz text missing marker:\n%s", txt)
	}
}

func TestSystemStatusJSONShape(t *testing.T) {
	b, _ := json.Marshal(buildSystemStatus("firecracker", true, "/s.sock", "1.2.3"))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"backend", "daemonRunning"} {
		if _, ok := m[k]; !ok {
			t.Errorf("json missing key %q (%s)", k, b)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run 'TestBuildSystemStatus|TestSystemStatus' -v` → FAIL.

- [ ] **Step 3: Add to `internal/cli/system.go`** (imports `encoding/json`, `strings`, `internal/firecracker`, `internal/server`):

```go
type systemStatus struct {
	Backend       string `json:"backend"`
	DaemonRunning bool   `json:"daemonRunning"`
	Socket        string `json:"socket,omitempty"`
	Version       string `json:"version,omitempty"`
}

func buildSystemStatus(backend string, daemonUp bool, socket, version string) systemStatus {
	return systemStatus{Backend: backend, DaemonRunning: daemonUp, Socket: socket, Version: version}
}

// renderSystemStatusText emits the container-ecosystem load-bearing substrings
// so container-dashboard's parser reads mvm unchanged. applevz has no daemon,
// so it says so honestly.
func renderSystemStatusText(s systemStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "backend: %s\n", s.Backend)
	if s.Backend == "applevz" {
		b.WriteString("applevz backend — no daemon required\n")
		fmt.Fprintf(&b, "application install root: %s\n", firecracker.DataDir())
		return b.String()
	}
	if s.DaemonRunning {
		b.WriteString("mvm daemon is running\n")
		fmt.Fprintf(&b, "container-apiserver version: %s\n", s.Version)
		fmt.Fprintf(&b, "application install root: %s\n", firecracker.DataDir())
		if s.Socket != "" {
			fmt.Fprintf(&b, "socket: %s\n", s.Socket)
		}
	} else {
		b.WriteString("mvm daemon is not running\n  start with: mvm system start\n")
	}
	return b.String()
}

func newSystemStatusCmd(limaClient *lima.Client, store *state.Store, version string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show system and daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			backend := store.GetBackend()
			daemonUp, socket := false, ""
			if backend == "firecracker" {
				c := server.DefaultClient()
				daemonUp = c.IsAvailable()
				socket = server.DefaultSocketPath()
			}
			st := buildSystemStatus(backend, daemonUp, socket, version)
			if format == "json" {
				data, err := json.MarshalIndent(st, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), renderSystemStatusText(st))
			fmt.Fprintln(cmd.OutOrStdout())
			return runDoctor(limaClient, store)
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}
```

- [ ] **Step 4: Wire `status` into the parent** — add `newSystemStatusCmd(limaClient, store, version),` to `newSystemCmd`'s `AddCommand` (the `version` string is already a `newSystemCmd` parameter). Extend `TestSystemSubcommandsRegistered`'s slice to include `"status"`.

- [ ] **Step 5: Verify + commit**

Run: `go test ./internal/cli/ -run 'TestBuildSystemStatus|TestSystemStatus|TestSystem' -v && go build ./...`

```bash
git add internal/cli/system.go internal/cli/system_test.go
git commit -m "feat(cli): backend-aware system status folding in doctor diagnostics"
```

---

### Task 19: `system df` — `--format json` → `cfDiskUsage` (source C6)

**Files:** Modify `internal/cli/system.go`, `internal/cli/system_test.go`.

**Interfaces:** `newSystemDFCmd(store *state.Store)`; `type resourceItem`; `buildDiskUsage(containers, images []resourceItem) cfDiskUsage`; `diskEntry([]resourceItem) cfDiskEntry`; `collectDiskUsage(ctx, store) (containers, images []resourceItem, err error)`. Consumes `cfDiskUsage`/`cfDiskEntry` (**Task 2**).

- [ ] **Step 1: Write the failing tests** — append to `internal/cli/system_test.go`:

```go
func TestBuildDiskUsageComputes(t *testing.T) {
	du := buildDiskUsage(
		[]resourceItem{{InUse: true, Bytes: 100}, {InUse: false, Bytes: 40}},
		[]resourceItem{{InUse: true, Bytes: 10}, {InUse: false, Bytes: 5}},
	)
	if du.Containers.Active != 1 || du.Containers.Total != 2 {
		t.Errorf("containers active/total = %d/%d, want 1/2", du.Containers.Active, du.Containers.Total)
	}
	if du.Containers.SizeInBytes != 140 || du.Containers.Reclaimable != 40 {
		t.Errorf("containers size/reclaimable = %d/%d, want 140/40", du.Containers.SizeInBytes, du.Containers.Reclaimable)
	}
	if du.Images.SizeInBytes != 15 || du.Images.Reclaimable != 5 {
		t.Errorf("images size/reclaimable = %d/%d, want 15/5", du.Images.SizeInBytes, du.Images.Reclaimable)
	}
	if du.Volumes != (cfDiskEntry{}) {
		t.Errorf("volumes = %+v, want zero until Slice 2", du.Volumes)
	}
}

func TestSystemDFJSONShape(t *testing.T) {
	b, _ := json.Marshal(buildDiskUsage(nil, nil))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"containers", "images", "volumes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("df json missing %q (%s)", k, b)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run 'TestBuildDiskUsage|TestSystemDF' -v` → FAIL.

- [ ] **Step 3: Add to `internal/cli/system.go`** (imports `context`, `text/tabwriter`; `os` already present):

```go
type resourceItem struct {
	InUse bool
	Bytes uint64
}

func diskEntry(items []resourceItem) cfDiskEntry {
	var e cfDiskEntry
	for _, it := range items {
		e.Total++
		e.SizeInBytes += it.Bytes
		if it.InUse {
			e.Active++
		} else {
			e.Reclaimable += it.Bytes
		}
	}
	return e
}

func buildDiskUsage(containers, images []resourceItem) cfDiskUsage {
	return cfDiskUsage{Containers: diskEntry(containers), Images: diskEntry(images), Volumes: cfDiskEntry{}}
}

func newSystemDFCmd(store *state.Store) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "df",
		Short: "Show mvm disk usage (VMs, images, volumes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, images, err := collectDiskUsage(cmd.Context(), store)
			if err != nil {
				return err
			}
			du := buildDiskUsage(containers, images)
			if format == "json" {
				data, err := json.MarshalIndent(du, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "TYPE\tACTIVE\tTOTAL\tSIZE\tRECLAIMABLE")
			fmt.Fprintf(w, "Containers\t%d\t%d\t%d\t%d\n", du.Containers.Active, du.Containers.Total, du.Containers.SizeInBytes, du.Containers.Reclaimable)
			fmt.Fprintf(w, "Images\t%d\t%d\t%d\t%d\n", du.Images.Active, du.Images.Total, du.Images.SizeInBytes, du.Images.Reclaimable)
			fmt.Fprintf(w, "Volumes\t%d\t%d\t%d\t%d\n", du.Volumes.Active, du.Volumes.Total, du.Volumes.SizeInBytes, du.Volumes.Reclaimable)
			w.Flush()
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}

// collectDiskUsage gathers VM rootfs sizes (from the store) and image sizes
// (from the daemon, best-effort — an applevz-only host has no daemon). A
// container is in use when running; an image when a VM spec references it.
func collectDiskUsage(ctx context.Context, store *state.Store) (containers, images []resourceItem, err error) {
	vms, err := store.ListVMs()
	if err != nil {
		return nil, nil, err
	}
	inUseImage := make(map[string]bool)
	for _, vm := range vms {
		var b uint64
		if fi, statErr := os.Stat(vm.RootfsPath); statErr == nil {
			b = uint64(fi.Size())
		}
		containers = append(containers, resourceItem{InUse: vm.Status == "running", Bytes: b})
		if vm.Spec != nil && vm.Spec.Image != "" {
			inUseImage[vm.Spec.Image] = true
		}
	}
	if sc, derr := requireDaemon(); derr == nil {
		if imgs, lerr := sc.ImageList(ctx); lerr == nil {
			for _, img := range imgs {
				images = append(images, resourceItem{InUse: inUseImage[img.Name], Bytes: uint64(img.SizeMB) * 1024 * 1024})
			}
		}
	}
	return containers, images, nil
}
```

- [ ] **Step 4: Wire `df` into the parent** — add `newSystemDFCmd(store),` to `newSystemCmd`'s `AddCommand`. Extend `TestSystemSubcommandsRegistered`'s slice to include `"df"`.

- [ ] **Step 5: Verify + commit**

Run: `go test ./internal/cli/ -run 'TestBuildDiskUsage|TestSystemDF|TestSystem' -v && go build ./...`

```bash
git add internal/cli/system.go internal/cli/system_test.go
git commit -m "feat(cli): add system df with container-shaped cfDiskUsage json"
```

---

## Whole-slice verification (after Task 19)

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | tail -30
```
Expected: clean build, vet silent, every package `ok`.

**Manual surface confirmation** (breaking-change check): the new/changed verbs and nouns work — `mvm create box`, `mvm start box`, `mvm exec box true`, `mvm kill box`, `mvm delete box`, `mvm list` (running-only), `mvm list -a`, `mvm list --format json`, `mvm inspect box`, `mvm stats --format json`, `mvm image ls --format json`, `mvm system status`, `mvm system df --format json`, `mvm system version` — and the removed surface is gone: `mvm images`, `mvm serve`, `mvm doctor`, `mvm version`, and `mvm exec box -- true` (the `--` form) no longer work. `scripts/integration-test.sh` passes against the new surface.

**Deferred to later slices (noted, not gaps):** Firecracker cumulative CPU end-to-end (additive daemon stats endpoint — Slice 3); guest-internal `numProcesses` (Slice 3); real `volume`/`network` nouns (Slices 2/3); image digests (Slice 3).


