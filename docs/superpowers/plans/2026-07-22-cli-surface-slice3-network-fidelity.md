# mvm CLI Surface Redesign — Slice 3 (Network + Fidelity) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Finish the container-surface read-views: a read-only `network` noun, real Firecracker cumulative-CPU in `stats`, and image digests in `image inspect`.

**Architecture:** All CLI + daemon, fully unit-testable (no live VM boot required). The `network` noun synthesizes mvm's single implicit "default" network from real constants. Firecracker cumulative CPU adds an additive `CPUUsageUsec` field to `VMStats` populated daemon-side from `ps -o time=`. Image digests add an additive `Digest` to `ImageInfo`, computed on-demand (sha256 of the ext4) by a new `GET /images/{name}` inspect endpoint.

**Tech Stack:** Go 1.22+, cobra, stdlib `net/http`/`encoding/json`/`crypto/sha256`, stdlib-only tests.

## Global Constraints

- Module `github.com/agentstep/mvm`; run from `/Users/paulmeller/Projects/firecracker`.
- Adding a NEW `omitempty` field to `VMStats`/`ImageInfo` is allowed (additive, backward-compatible — old SDK clients ignore it). Do NOT change or remove existing fields; do NOT touch `sdk/`.
- Container output shapes follow `docs/container-compat-matrix.md`: `network list` → array of `{id, state, config.mode, status.ipv4Subnet}`; empty→`[]`; camelCase (`ipv4Subnet`).
- **Honesty rule for applevz networking:** applevz uses Apple's NAT, which assigns the subnet dynamically per machine — mvm does NOT own it. So on applevz, `status.ipv4Subnet` is left EMPTY (never hardcode `192.168.65.0/24`). Firecracker's subnet is the real fixed constant `172.16.0.0/24`.
- Stdlib-only tests, end commits with the `Claude-Session:` trailer.

## File structure

- **Create:** `internal/cli/network.go` (+ test); `internal/server/routes.go` gains `handleImageInspect`.
- **Modify:** `internal/cli/containerfmt.go` (`cfNetwork` + `defaultNetwork`); `internal/firecracker/stats.go` (`ProcessCumulativeCPU`); `internal/server/routes.go` (`VMStats.CPUUsageUsec`, `ImageInfo.Digest`, `handleStatsVMs`, `handleImageInspect`); `internal/server/server.go` (route); `internal/server/client.go` (`ImageInspect`); `internal/cli/stats.go` (consume `CPUUsageUsec`); `internal/cli/image.go` (`runImageInspect`); `internal/cli/root.go` (register `network`).

## Execution order

Task 1 (cfNetwork) → Task 2 (network noun) → Task 3 (daemon cumulative CPU) → Task 4 (CLI stats consumes it) → Task 5 (daemon image-inspect digest) → Task 6 (CLI image inspect consumes it) → Task 7 (verification).

---

### Task 1: `cfNetwork` presentation shape + `defaultNetwork` builder

**Files:** Modify `internal/cli/containerfmt.go`; Test `internal/cli/containerfmt_test.go`.

**Interfaces:** Produces `cfNetwork`/`cfNetworkConfig`/`cfNetworkStatus` and `func defaultNetwork(backend string) cfNetwork`. Task 2 consumes them.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/containerfmt_test.go`:

```go
func TestDefaultNetworkFirecracker(t *testing.T) {
	got := mustJSON(t, defaultNetwork("firecracker"))
	want := `{
  "id": "default",
  "state": "running",
  "config": {
    "mode": "nat"
  },
  "status": {
    "ipv4Subnet": "172.16.0.0/24"
  }
}`
	if got != want {
		t.Errorf("firecracker default network:\n got:\n%s\n want:\n%s", got, want)
	}
}

func TestDefaultNetworkApplevzHasNoSubnet(t *testing.T) {
	n := defaultNetwork("applevz")
	if n.Config.Mode != "nat" || n.ID != "default" {
		t.Errorf("applevz network = %+v, want id=default mode=nat", n)
	}
	if n.Status.IPv4Subnet != "" {
		t.Errorf("applevz ipv4Subnet = %q, want empty (Apple NAT assigns it dynamically)", n.Status.IPv4Subnet)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run TestDefaultNetwork -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Add to `internal/cli/containerfmt.go`**

```go
type cfNetworkConfig struct {
	Mode string `json:"mode"`
}
type cfNetworkStatus struct {
	IPv4Subnet string `json:"ipv4Subnet"`
}
type cfNetwork struct {
	ID     string          `json:"id"`
	State  string          `json:"state"`
	Config cfNetworkConfig `json:"config"`
	Status cfNetworkStatus `json:"status"`
}

// defaultNetwork synthesizes mvm's single implicit "default" network. mvm has
// no user-defined networks — each VM gets a per-VM /30 out of a fixed scheme —
// so this reports the umbrella network honestly. Firecracker's subnet is the
// real fixed constant (172.16.0.0/24, see internal/state/network.go); applevz
// uses Apple's NAT, which assigns the subnet dynamically per machine, so its
// subnet is left empty rather than faked.
func defaultNetwork(backend string) cfNetwork {
	n := cfNetwork{ID: "default", State: "running", Config: cfNetworkConfig{Mode: "nat"}}
	if backend == "firecracker" {
		n.Status.IPv4Subnet = "172.16.0.0/24"
	}
	return n
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/cli/ -run TestDefaultNetwork -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/containerfmt.go internal/cli/containerfmt_test.go
git commit -m "feat(cli): cfNetwork shape + defaultNetwork builder (FC subnet real, applevz empty)"
```

---

### Task 2: `network` noun (`ls`/`inspect`, read-only)

**Files:** Create `internal/cli/network.go`, `internal/cli/network_test.go`; Modify `internal/cli/root.go`.

**Interfaces:** Consumes `defaultNetwork` (Task 1), `store.GetBackend()`.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/network_test.go`:

```go
package cli

import (
	"path/filepath"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

func TestNetworkCmdWiring(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	c := newNetworkCmd(store)
	if c.Use != "network" {
		t.Fatalf("Use = %q, want network", c.Use)
	}
	names := map[string]bool{}
	for _, sub := range c.Commands() {
		names[sub.Name()] = true
	}
	if !names["ls"] || !names["inspect"] {
		t.Fatalf("subcommands = %v, want ls+inspect", names)
	}
	ls, _, _ := c.Find([]string{"ls"})
	if ls.Flags().Lookup("format") == nil {
		t.Error("ls missing --format")
	}
}

func TestNetworkInspectUnknownErrors(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")
	if err := runNetworkInspect(store, "nope"); err == nil {
		t.Error("inspect of a non-default network should error (only 'default' exists)")
	}
	if err := runNetworkInspect(store, "default"); err != nil {
		t.Errorf("inspect default: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run TestNetwork -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Create `internal/cli/network.go`**

```go
package cli

import (
	"encoding/json"
	"fmt"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newNetworkCmd(store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Inspect mvm networks (read-only; mvm has one implicit default network)",
	}
	cmd.AddCommand(newNetworkLsCmd(store), newNetworkInspectCmd(store))
	return cmd
}

func newNetworkLsCmd(store *state.Store) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List networks",
		Aliases: []string{"list"},
		RunE:    func(cmd *cobra.Command, args []string) error { return runNetworkLs(store, format) },
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}

func newNetworkInspectCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show network details (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runNetworkInspect(store, args[0]) },
	}
}

func runNetworkLs(store *state.Store, format string) error {
	// mvm has exactly one network: the implicit "default".
	nets := []cfNetwork{defaultNetwork(store.GetBackend())}
	if format == "json" {
		data, err := json.MarshalIndent(nets, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("%-10s %-8s %-6s %s\n", "ID", "STATE", "MODE", "SUBNET")
	for _, n := range nets {
		fmt.Printf("%-10s %-8s %-6s %s\n", n.ID, n.State, n.Config.Mode, n.Status.IPv4Subnet)
	}
	return nil
}

func runNetworkInspect(store *state.Store, name string) error {
	if name != "default" {
		return fmt.Errorf("no network named %q (mvm has one network: default)", name)
	}
	data, err := json.MarshalIndent(defaultNetwork(store.GetBackend()), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
```

- [ ] **Step 4: Register in `internal/cli/root.go`**

Add `newNetworkCmd(store),` to the `rootCmd.AddCommand(...)` block (after `newVolumeCmd`/`newImageCmd` — place it after `newImageCmd(store),`).

- [ ] **Step 5: Run to verify pass + commit**

Run: `go test ./internal/cli/ -run TestNetwork -v && go build ./...`

```bash
git add internal/cli/network.go internal/cli/network_test.go internal/cli/root.go
git commit -m "feat(cli): read-only network noun (ls/inspect) — the implicit default network"
```

---

### Task 3: Firecracker cumulative CPU — daemon side

**Files:** Modify `internal/firecracker/stats.go`, `internal/server/routes.go`; Test `internal/firecracker/stats_test.go`, `internal/server/routes_test.go`.

**Interfaces:** Produces `firecracker.ProcessCumulativeCPU(ex, pid) (uint64, error)` and the additive `VMStats.CPUUsageUsec` field. Task 4 consumes the field.

- [ ] **Step 1: Write the failing test**

Append to `internal/firecracker/stats_test.go` (reuse the `captureExecutor` pattern or a small inline mock returning a ps line):

```go
type fixedExec struct{ out string }

func (f fixedExec) Run(string) (string, error)                       { return f.out, nil }
func (f fixedExec) RunWithTimeout(string, time.Duration) (string, error) { return f.out, nil }

func TestProcessCumulativeCPU(t *testing.T) {
	// `ps -o time=,rss=` → cumulative CPU time + rss.
	usec, err := ProcessCumulativeCPU(fixedExec{out: "  00:12.50 102400\n"}, 4242)
	if err != nil {
		t.Fatalf("ProcessCumulativeCPU: %v", err)
	}
	if usec != 12_500_000 {
		t.Errorf("cpuUsec = %d, want 12500000", usec)
	}
}
```

(Add `"time"` to the test imports if not present.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/firecracker/ -run TestProcessCumulativeCPU -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Add `ProcessCumulativeCPU` to `internal/firecracker/stats.go`**

```go
// ProcessCumulativeCPU returns a process's CUMULATIVE CPU time in microseconds
// (monotonic — the value cfStats.cpuUsageUsec needs, which the dashboard deltas
// across samples). It reads `ps -o time=,rss=` and parses via ParseCumulativePS,
// discarding the memory field. Used by the daemon's stats handler so the
// Firecracker path reports real cumulative CPU instead of 0.
func ProcessCumulativeCPU(ex Executor, pid int) (uint64, error) {
	out, err := ex.RunWithTimeout(fmt.Sprintf("ps -o time=,rss= -p %d", pid), 10*time.Second)
	if err != nil {
		return 0, err
	}
	usec, _, err := ParseCumulativePS(out)
	return usec, err
}
```

(`fmt` and `time` are already imported in `stats.go`.)

- [ ] **Step 4: Add the field + populate it in `handleStatsVMs`**

In `internal/server/routes.go`, add to `VMStats` (after `MemMB`):

```go
	CPUUsageUsec uint64  `json:"cpu_usage_usec,omitempty"` // cumulative CPU microseconds (monotonic)
```

In `handleStatsVMs`, inside the `if vm.Status == "running" && vm.PID > 0` block, after the existing `ProcessStats` success sets `st.CPUPct, st.MemMB`, add a best-effort cumulative read:

```go
			} else {
				st.CPUPct, st.MemMB = cpu, memMB
				if usec, cerr := firecracker.ProcessCumulativeCPU(s.executor, vm.PID); cerr == nil {
					st.CPUUsageUsec = usec
				}
			}
```

- [ ] **Step 5: Add a route test**

Append to `internal/server/routes_test.go`:

```go
func TestHandleStatsVMsReportsCumulativeCPU(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "web", Status: "running", Backend: "firecracker", PID: 4242, CreatedAt: time.Now()})
	s.executor = &mockExecutor{runFunc: func(command string) (string, error) {
		// Both ProcessStats and ProcessCumulativeCPU shell ps; return a line
		// that ParseCumulativePS parses to a nonzero cumulative µs.
		return "  00:30.00 51200", nil
	}}
	req := httptest.NewRequest("GET", "/vms/stats", nil)
	w := httptest.NewRecorder()
	s.handleStatsVMs(w, req)
	var got []VMStats
	json.NewDecoder(w.Body).Decode(&got)
	if len(got) != 1 {
		t.Fatalf("stats len = %d, want 1", len(got))
	}
	if got[0].CPUUsageUsec == 0 {
		t.Errorf("CPUUsageUsec = 0, want nonzero cumulative µs")
	}
}
```

- [ ] **Step 6: Run + commit**

Run: `go test ./internal/firecracker/ ./internal/server/ -run 'Cumulative|StatsVMs' -v && go build ./...`

```bash
git add internal/firecracker/stats.go internal/firecracker/stats_test.go internal/server/routes.go internal/server/routes_test.go
git commit -m "feat(server): daemon reports cumulative CPU microseconds in VMStats"
```

---

### Task 4: CLI `stats` consumes the daemon cumulative CPU

**Files:** Modify `internal/cli/stats.go`; Test `internal/cli/stats_test.go`.

**Interfaces:** Consumes `server.VMStats.CPUUsageUsec` (Task 3).

- [ ] **Step 1: Update the daemon branch**

In `internal/cli/stats.go`'s `runStats`, the daemon (Firecracker) loop currently builds a `cfStatSource` with `CPUUsageUsec` left at 0. Set it from the field:

```go
			sources = append(sources, cfStatSource{
				Name: s.Name, Backend: s.Backend, PID: s.PID, Status: s.Status,
				CPUUsageUsec:     s.CPUUsageUsec,
				MemoryUsageBytes: uint64(s.MemMB * 1024 * 1024),
				MemoryLimitBytes: memLimitBytes(specs[s.Name]),
				NumProcesses:     1,
			})
```

(The human table at the existing `cpu = fmt.Sprintf("%.1f", float64(s.CPUUsageUsec)/1e6)` line then shows a real value for running Firecracker VMs — fixing the Slice-1 `CPU=0.0` minor automatically.)

- [ ] **Step 2: Test**

Append to `internal/cli/stats_test.go`:

```go
func TestStatsFCSourceCarriesCumulativeCPU(t *testing.T) {
	// A daemon VMStats with a cumulative µs value must flow into cfStatSource
	// and out as cpuUsageUsec (not dropped to 0 as in Slice 1).
	vs := server.VMStats{Name: "web", Backend: "firecracker", PID: 1, Status: "running", CPUUsageUsec: 9_000_000, MemMB: 100}
	src := cfStatSource{
		Name: vs.Name, Backend: vs.Backend, PID: vs.PID, Status: vs.Status,
		CPUUsageUsec:     vs.CPUUsageUsec,
		MemoryUsageBytes: uint64(vs.MemMB * 1024 * 1024),
		NumProcesses:     1,
	}
	out := toCFStats([]cfStatSource{src})
	if out[0].CPUUsageUsec != 9_000_000 {
		t.Errorf("cpuUsageUsec = %d, want 9000000", out[0].CPUUsageUsec)
	}
}
```

(Ensure `server` is imported in `stats_test.go`.)

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/cli/ -run 'Stats' -v && go build ./...`

```bash
git add internal/cli/stats.go internal/cli/stats_test.go
git commit -m "feat(cli): stats uses the daemon's cumulative CPU for Firecracker VMs"
```

---

### Task 5: Image digest — daemon `GET /images/{name}` inspect endpoint

**Files:** Modify `internal/server/routes.go`, `internal/server/server.go`, `internal/server/client.go`; Test `internal/server/routes_test.go`.

**Interfaces:** Produces the additive `ImageInfo.Digest`, `handleImageInspect`, `Client.ImageInspect(ctx, name) (*ImageInfo, error)`. Task 6 consumes the client method.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/routes_test.go`:

```go
func TestHandleImageInspectComputesDigest(t *testing.T) {
	s, _ := testServer(t)
	t.Setenv("MVM_DATA_DIR", t.TempDir())
	if err := os.MkdirAll(firecracker.CacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firecracker.CacheDir(), "web.ext4"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	req := httptest.NewRequest("GET", "/v1/images/web", nil)
	req.SetPathValue("name", "web")
	w := httptest.NewRecorder()
	s.handleImageInspect(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var info ImageInfo
	json.NewDecoder(w.Body).Decode(&info)
	if info.Name != "web" || info.SizeMB != 0 {
		t.Errorf("info = %+v", info)
	}
	if info.Digest != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("digest = %q", info.Digest)
	}
}

func TestHandleImageInspectRejectsTraversal(t *testing.T) {
	s, _ := testServer(t)
	req := httptest.NewRequest("GET", "/v1/images/x", nil)
	req.SetPathValue("name", "../../etc/passwd")
	w := httptest.NewRecorder()
	s.handleImageInspect(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for traversal name", w.Code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/server/ -run TestHandleImageInspect -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Add the field + handler**

Add to `ImageInfo` in `internal/server/routes.go`:

```go
	Digest string `json:"digest,omitempty"` // "sha256:<hex>", computed on inspect
```

Add the handler (after `handleImageDelete`). Add `"crypto/sha256"`, `"encoding/hex"`, `"io"` to the import block if not present:

```go
func (s *Server) handleImageInspect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := state.ValidateName(name); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	path := firecracker.CacheDir() + "/" + name + ".ext4"
	fi, err := os.Stat(path)
	if err != nil {
		httpError(w, fmt.Errorf("image %q not found", name), http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ImageInfo{
		Name:   name,
		SizeMB: int(fi.Size() / (1024 * 1024)),
		Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)),
	})
}
```

- [ ] **Step 4: Register + client method**

In `internal/server/server.go` `buildMux`, after the image routes:

```go
	register("GET", "/images/{name}", s.handleImageInspect)
```

In `internal/server/client.go` (after `ImageDelete`):

```go
// ImageInspect fetches one image's info including its sha256 digest (computed
// on-demand by the daemon).
func (c *Client) ImageInspect(ctx context.Context, name string) (*ImageInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url(fmt.Sprintf("/v1/images/%s", name)), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var info ImageInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}
```

- [ ] **Step 5: Run + commit**

Run: `go test ./internal/server/ -run TestHandleImageInspect -v && go build ./...`
Also confirm no route collision: `GET /images/{name}` vs `GET /images/{name}/download` — distinct patterns, but run `go test ./internal/server/ -run TestHandleImage` to confirm existing image tests still pass.

```bash
git add internal/server/routes.go internal/server/server.go internal/server/client.go internal/server/routes_test.go
git commit -m "feat(server): GET /images/{name} inspect endpoint with on-demand sha256 digest"
```

---

### Task 6: CLI `image inspect` uses the real digest

**Files:** Modify `internal/cli/image.go`; Test `internal/cli/image_test.go`.

**Interfaces:** Consumes `Client.ImageInspect` (Task 5).

- [ ] **Step 1: Update `runImageInspect`**

Replace the list+find body with a direct inspect call so the digest is populated:

```go
func runImageInspect(ctx context.Context, name string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	info, err := sc.ImageInspect(ctx, name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(imageToCF(*info), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
```

And update `imageToCF` to carry the digest through (it currently hardcodes `Digest: ""`):

```go
func imageToCF(img server.ImageInfo) cfImage {
	return cfImage{
		Reference: img.Name,
		Descriptor: cfDescriptor{
			Digest: img.Digest, // populated by image inspect; empty on list
			Size:   int64(img.SizeMB) * 1024 * 1024,
		},
	}
}
```

- [ ] **Step 2: Test**

Append to `internal/cli/image_test.go`:

```go
func TestImageToCFCarriesDigest(t *testing.T) {
	cf := imageToCF(server.ImageInfo{Name: "web", SizeMB: 64, Digest: "sha256:abc"})
	if cf.Descriptor.Digest != "sha256:abc" {
		t.Errorf("digest = %q, want sha256:abc", cf.Descriptor.Digest)
	}
	// list path (no digest) stays empty — dashboard tolerates it.
	cf2 := imageToCF(server.ImageInfo{Name: "db", SizeMB: 10})
	if cf2.Descriptor.Digest != "" {
		t.Errorf("no-digest image digest = %q, want empty", cf2.Descriptor.Digest)
	}
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/cli/ -run 'TestImage' -v && go build ./...`

```bash
git add internal/cli/image.go internal/cli/image_test.go
git commit -m "feat(cli): image inspect shows the real sha256 digest via the daemon"
```

---

### Task 7: Whole-slice verification

**Files:** none.

- [ ] **Step 1: Full build + vet + test**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -20`
Expected: clean, every package `ok`.

- [ ] **Step 2: Surface confirmation**

`mvm network ls --format json` → one `default` network (subnet `172.16.0.0/24` on a firecracker host, empty on applevz); `mvm network inspect default` → the object; `mvm network inspect other` → error. `mvm stats --format json` on a running Firecracker VM → nonzero `cpuUsageUsec`. `mvm image inspect <name>` → a `descriptor.digest` of `sha256:…`.

- [ ] **Step 3: Commit (only if Steps 1-2 needed a fix)**

If everything passed, skip.

---

## Deferred (not this slice)

Real named volumes (Slice 2, deferred — see `docs/superpowers/specs/2026-07-21-slice2-volumes-design.md`); guest-internal `numProcesses` in stats; the stats `parseCPUTime` sub-µs rounding (negligible). Image `ls` leaves digest empty by design (computing sha256 for every image on every list is wasteful; the dashboard tolerates empty and only `inspect` needs it).
