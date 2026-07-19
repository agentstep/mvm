# Container Ergonomics Alignment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the six small flag/command additions from the "Compatibility Policy" of `docs/superpowers/specs/2026-07-19-image-vm-organization-design.md` (`--env-file` on exec, `--rm` on start, `[host-ip:]` in `-p`, `-d`/`--detach` on exec, `mvm stats`, `--format json|table` on `inspect`/`list`) — the flags Docker/Apple-`container` users expect, adopted "where it is cheap and inherits an ecosystem," without touching mvm's sandbox differentiators (pause/snapshot/pool/secrets/net-policy) or changing any existing command's default output.

**Architecture:** Every task is a thin, additive slice through the existing three layers — `internal/cli` (cobra commands, thin), `internal/server` (daemon HTTP API + `internal/server.Client`, the CLI's HTTP client to it), `internal/firecracker` / `internal/vm` (the two backend implementations). No task introduces a new subsystem. Two tasks touch the wire schema (`state.PortMap` gains `HostIP`; a new `server.VMStats` type is introduced) — both additive, and Task 6's addition is verified against `internal/server/schema_golden_test.go`'s existing golden-JSON convention.

**Tech Stack:** Go 1.22+, cobra, stdlib-only tests (no testify) — matches `internal/cli` and `internal/firecracker`'s existing conventions.

## Global Constraints

- **No behavior change to any existing command's default output or flags.** `--json` on `list` keeps working byte-for-byte identically; `inspect` with no flags keeps emitting exactly the JSON it does today. Every new flag is opt-in.
- **JSON is additive-only.** `state.PortMap` gains one new `omitempty` field (`HostIP`). `internal/server/schema_golden_test.go`'s existing goldens (`TestVMResponseSchemaGolden`, `TestVMInspectResponseSchemaGolden`) assert only the *top-level* key sets of `VMResponse`/`VMInspectResponse`/`VMSpec` — none of them enumerate `PortMap`'s own fields (they call `jsonKeys` on the outer struct, which only ever returns that struct's direct keys; a slice-valued field like `"ports"` never gets its element shape inspected). **Grounded finding: adding `HostIP` to `PortMap` does not require editing any existing golden — verified by reading `schema_golden_test.go`'s `jsonKeys` helper, which flattens only one level.** Task 4 still re-runs the golden suite as a checked step, and Task 6 (new `mvm stats` response type) *adds* a new golden test, per the same convention, since that's a wholly new top-level schema.
- **`mvm run`'s ephemeral-by-default semantics are untouched.** `--rm` is rejected on `start`, not added to `run` — `run` already has `--rm`-equivalent behavior (ephemeral unless `--name` is given); see Task 3.
- Match existing code style: tabs, stdlib-only tests, matching every other file in `internal/cli` / `internal/firecracker` / `internal/server`.
- Repo module path is `github.com/agentstep/mvm`. Run all commands from `/Users/paulmeller/Projects/firecracker`.

---

### Task 1: `--env-file` on `mvm exec` and `mvm run`

**Files:**
- Create: `internal/cli/envfile.go`
- Test: `internal/cli/envfile_test.go`
- Modify: `internal/cli/exec.go` (`newExecCmd`)
- Modify: `internal/cli/run.go` (`newRunCmd`)

**Interfaces:**
- Produces: `func parseEnvFile(path string) ([]string, error)`, `func mergeEnvFile(path string, explicit []string) ([]string, error)`.
- Consumes: nothing new — `runExec` and `runRun` already take `envVars []string`; `mergeEnvFile`'s output is passed straight into the existing parameter, so neither function's signature changes.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/envfile_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// === parseEnvFile ===

func TestParseEnvFileSkipsBlankAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	content := "FOO=bar\n\n# a comment\nBAZ=qux\n  \n#another\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	want := []string{"FOO=bar", "BAZ=qux"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parseEnvFile() = %v, want %v", got, want)
	}
}

func TestParseEnvFileRejectsBareKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("FOO=bar\nNOTKEYVALUE\n"), 0o644)
	if _, err := parseEnvFile(path); err == nil {
		t.Fatal("parseEnvFile() = nil error, want error for a bare-key line")
	}
}

func TestParseEnvFileMissingFile(t *testing.T) {
	if _, err := parseEnvFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("parseEnvFile() = nil error, want error for a missing file")
	}
}

func TestParseEnvFilePreservesEqualsInValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("URL=https://example.com/?a=b\n"), 0o644)
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	if len(got) != 1 || got[0] != "URL=https://example.com/?a=b" {
		t.Errorf("parseEnvFile() = %v, want [URL=https://example.com/?a=b]", got)
	}
}

// === mergeEnvFile ===

func TestMergeEnvFileFileFirstExplicitWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("FOO=fromfile\nBAR=fromfile\n"), 0o644)
	got, err := mergeEnvFile(path, []string{"FOO=fromflag"})
	if err != nil {
		t.Fatalf("mergeEnvFile: %v", err)
	}
	want := []string{"FOO=fromfile", "BAR=fromfile", "FOO=fromflag"}
	if len(got) != len(want) {
		t.Fatalf("mergeEnvFile() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mergeEnvFile()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMergeEnvFileEmptyPathPassthrough(t *testing.T) {
	got, err := mergeEnvFile("", []string{"A=b"})
	if err != nil {
		t.Fatalf("mergeEnvFile: %v", err)
	}
	if len(got) != 1 || got[0] != "A=b" {
		t.Errorf("mergeEnvFile(\"\", ...) = %v, want unchanged passthrough", got)
	}
}

func TestMergeEnvFilePropagatesParseError(t *testing.T) {
	if _, err := mergeEnvFile(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("mergeEnvFile() = nil error, want the missing-file error propagated")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestParseEnvFile|TestMergeEnvFile' -v`
Expected: FAIL — compile errors `undefined: parseEnvFile`, `undefined: mergeEnvFile`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/envfile.go`:

```go
package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// parseEnvFile reads KEY=VALUE lines from path, skipping blank lines and
// full-line comments (a leading '#'), matching Apple container's documented
// --env-file format. Each valid line is returned as a "KEY=VALUE" string in
// the exact shape mvm exec/run's existing -e/--env flag already produces, so
// callers just append the result to their envVars slice.
//
// Unlike Docker's --env-file, a bare KEY (no '=') is a hard error rather
// than silently inheriting the value from mvm's own host environment —
// leaking unrelated host state into the guest is exactly the class of leak
// mvm's sandbox model exists to prevent, so it is never done implicitly.
func parseEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read env file %q: %w", path, err)
	}
	defer f.Close()

	var result []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			return nil, fmt.Errorf("%s:%d: invalid line %q (want KEY=VALUE)", path, lineNum, line)
		}
		result = append(result, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file %q: %w", path, err)
	}
	return result, nil
}

// mergeEnvFile combines --env-file entries with explicit -e/--env values,
// file entries first so explicit flags override same-key file entries —
// matching Docker's --env-file + -e precedence (later `export` wins in the
// shell script buildExecScript assembles). A no-op (returns explicit
// unchanged) when path is "".
func mergeEnvFile(path string, explicit []string) ([]string, error) {
	if path == "" {
		return explicit, nil
	}
	fileVars, err := parseEnvFile(path)
	if err != nil {
		return nil, err
	}
	return append(fileVars, explicit...), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestParseEnvFile|TestMergeEnvFile' -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Wire `--env-file` into `mvm exec`**

In `internal/cli/exec.go`, add `envFile` to `newExecCmd`'s var block and flag registration, and resolve it before calling `runExec`:

```go
func newExecCmd(store *state.Store) *cobra.Command {
	var (
		interactive bool
		tty         bool
		workdir     string
		envVars     []string
		envFile     string
		user        string
	)

	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a command in a running microVM",
		Long: `Run a command inside a running microVM.

  mvm exec my-vm -- ls /
  mvm exec my-vm -it -- bash
  mvm exec my-vm -e FOO=bar -- env
  mvm exec my-vm --env-file .env -- env
  echo "data" | mvm exec my-vm -- cat`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			remoteArgs := args[1:]
			allEnv, err := mergeEnvFile(envFile, envVars)
			if err != nil {
				return err
			}
			return runExec(store, name, remoteArgs, interactive || tty, workdir, allEnv, user)
		},
	}

	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the VM")
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil, "set environment variables (KEY=VALUE)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read environment variables from a file (KEY=VALUE per line, # comments and blank lines skipped)")
	cmd.Flags().StringVarP(&user, "user", "u", "", "run as user")

	return cmd
}
```

`runExec`'s signature and body are unchanged — `mergeEnvFile`'s output is just a richer `envVars` slice flowing into the same parameter it always took.

- [ ] **Step 6: Wire `--env-file` into `mvm run`**

In `internal/cli/run.go`'s `newRunCmd`, apply the identical pattern: add `envFile string` to the var block, a `--env-file` flag, and resolve inside `RunE` before calling `runRun`:

```go
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil, "set environment variables (KEY=VALUE, foreground command only)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read environment variables from a file (KEY=VALUE per line, foreground command only)")
```

and in `RunE`, before the `runRun` call:

```go
			allEnv, err := mergeEnvFile(envFile, envVars)
			if err != nil {
				return err
			}
			return runRun(store, image, cmdArgs, name, detach, cpus, memoryMB, netPolicy, portMaps, volumes, interactive || tty, workdir, allEnv, user)
```

(`envFile` is declared alongside `envVars` in the existing var block at the top of `newRunCmd`.) `runRun`'s signature is unchanged.

- [ ] **Step 7: Run the full package test + build**

Run: `go build ./... && go test ./internal/cli/ -v 2>&1 | tail -30`
Expected: clean build, no FAILs (existing `TestParsePorts`, `TestRunRunRejectsCustomImageOnAppleVZ`, etc. all still pass — nothing in this task touches their code paths).

- [ ] **Step 8: Manual smoke-test**

Run: `go run ./cmd/mvm exec --help` and `go run ./cmd/mvm run --help`
Expected: both show a `--env-file` flag in their usage output.

- [ ] **Step 9: Commit**

```bash
git add internal/cli/envfile.go internal/cli/envfile_test.go internal/cli/exec.go internal/cli/run.go
git commit -m "feat(cli): add --env-file to mvm exec and mvm run"
```

---

### Task 2: `--format json|table` on `mvm list` and `mvm inspect`

**Files:**
- Create: `internal/cli/format.go`
- Test: `internal/cli/format_test.go`
- Modify: `internal/cli/list.go`
- Modify: `internal/cli/inspect.go`

**Interfaces:**
- Produces: `func resolveFormat(format string, jsonFlag bool) (wantJSON bool, err error)` (used by `list`, which already has a `--json` bool to reconcile) and `func printInspectTable(resp server.VMInspectResponse) error` (used by `inspect`, which has no pre-existing `--json` bool — its default output was always JSON).
- Consumes: `server.VMInspectResponse`, `state.VM` — both existing, unmodified.

**Grounded correction:** the prompt's premise that `images.go` sets a `--format` precedent doesn't hold — `internal/cli/images.go` has no `--format`/`--json` flag at all (verified: `grep -rn '"format"' internal/cli/*.go` returns nothing anywhere in the codebase today). This task establishes the first `--format` flag from scratch, following the existing `outHuman`/`outJSON`/`outQuiet` `outputMode` pattern already used by `start`'s `--json` (`internal/cli/bootresult.go`) as the closest analogue instead.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/format_test.go`:

```go
package cli

import "testing"

func TestResolveFormatDefaultsToJSONFlag(t *testing.T) {
	if got, err := resolveFormat("", true); err != nil || !got {
		t.Errorf(`resolveFormat("", true) = %v, %v; want true, nil`, got, err)
	}
	if got, err := resolveFormat("", false); err != nil || got {
		t.Errorf(`resolveFormat("", false) = %v, %v; want false, nil`, got, err)
	}
}

func TestResolveFormatJSON(t *testing.T) {
	got, err := resolveFormat("json", false)
	if err != nil || !got {
		t.Errorf(`resolveFormat("json", false) = %v, %v; want true, nil`, got, err)
	}
}

func TestResolveFormatTable(t *testing.T) {
	got, err := resolveFormat("table", true)
	if err != nil || got {
		t.Errorf(`resolveFormat("table", true) = %v, %v; want false, nil`, got, err)
	}
}

func TestResolveFormatInvalid(t *testing.T) {
	if _, err := resolveFormat("yaml", false); err == nil {
		t.Fatal(`resolveFormat("yaml", false) = nil error, want error`)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestResolveFormat -v`
Expected: FAIL — compile error `undefined: resolveFormat`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/format.go`:

```go
package cli

import "fmt"

// resolveFormat reconciles the container-style --format flag with list's
// pre-existing --json boolean. --format is purely additive: leaving it at
// its zero value "" keeps --json working exactly as it always has (whatever
// jsonFlag says wins). A non-empty --format takes precedence over --json
// when both are given — deliberately simple rather than treating that
// combination as a conflict to reject, since a cosmetic double-flag isn't
// worth the extra cobra flag-changed tracking.
func resolveFormat(format string, jsonFlag bool) (wantJSON bool, err error) {
	switch format {
	case "":
		return jsonFlag, nil
	case "json":
		return true, nil
	case "table":
		return false, nil
	default:
		return false, fmt.Errorf("invalid --format %q (want %q or %q)", format, "json", "table")
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestResolveFormat -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Wire `--format` into `mvm list`**

In `internal/cli/list.go`, add a `format` var and flag, resolving it before calling the existing (unchanged) `runList`:

```go
func newListCmd(store *state.Store) *cobra.Command {
	var (
		jsonOutput bool
		quiet      bool
		format     string
	)

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List all microVMs",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := resolveFormat(format, jsonOutput)
			if err != nil {
				return err
			}
			return runList(store, wantJSON, quiet)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON (alias for --format json)")
	cmd.Flags().StringVar(&format, "format", "", "output format: table (default) or json")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "only print VM names")

	return cmd
}
```

`runList`'s signature and body are completely unchanged — it still just takes a `jsonOutput bool`.

- [ ] **Step 6: Add a human-table renderer and `--format` to `mvm inspect`**

`inspect` has no `--json` bool to reconcile — its historical default (no flags at all) has always been JSON, so its `--format` flag defaults to `"json"` explicitly (not `""`), preserving that default exactly while adding `table` as the opt-in human view. Replace `internal/cli/inspect.go` in full:

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newInspectCmd(store *state.Store) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "inspect <name>",
		Short: "Display detailed information about a microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(store, args[0], format)
		},
	}

	cmd.Flags().StringVar(&format, "format", "json", "output format: json (default) or table")

	return cmd
}

func runInspect(store *state.Store, name string, format string) error {
	if format != "json" && format != "table" {
		return fmt.Errorf("invalid --format %q (want %q or %q)", format, "json", "table")
	}

	// applevz VMs live purely in local state — the daemon has never heard
	// of them (same backend split as `mvm list`).
	if vm, err := store.GetVM(name); err == nil && vm.Backend == "applevz" {
		return printInspectResult(server.InspectResponseFromVM(vm), format)
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	resp, err := sc.InspectVM(context.Background(), name)
	if err != nil {
		return err
	}
	return printInspectResult(*resp, format)
}

func printInspectResult(resp server.VMInspectResponse, format string) error {
	if format == "table" {
		return printInspectTable(resp)
	}
	return printInspect(resp)
}

func printInspect(resp server.VMInspectResponse) error {
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// printInspectTable renders the same VMInspectResponse as a human-readable
// key: value summary — additive only, the JSON path (printInspect) is
// completely unchanged and stays the default.
func printInspectTable(resp server.VMInspectResponse) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Name:\t%s\n", resp.Name)
	fmt.Fprintf(w, "Status:\t%s\n", resp.Status)
	fmt.Fprintf(w, "Backend:\t%s\n", resp.Backend)
	fmt.Fprintf(w, "Guest IP:\t%s\n", resp.GuestIP)
	fmt.Fprintf(w, "PID:\t%d\n", resp.PID)
	fmt.Fprintf(w, "Created:\t%s\n", resp.CreatedAt.Format(time.RFC3339))
	if resp.Error != "" {
		fmt.Fprintf(w, "Error:\t%s\n", resp.Error)
	}
	for _, p := range resp.Ports {
		fmt.Fprintf(w, "Port:\t%d -> %d/%s\n", p.HostPort, p.GuestPort, p.Proto)
	}
	if resp.Spec != nil {
		fmt.Fprintf(w, "Cpus:\t%d\n", resp.Spec.Cpus)
		fmt.Fprintf(w, "Memory:\t%d MiB\n", resp.Spec.MemoryMB)
		fmt.Fprintf(w, "Net Policy:\t%s\n", resp.Spec.NetPolicy)
		if resp.Spec.Image != "" {
			fmt.Fprintf(w, "Image:\t%s\n", resp.Spec.Image)
		}
	}
	return w.Flush()
}
```

(The `Port:` line here prints `HostPort -> GuestPort/Proto` with no host-IP column yet — `state.PortMap` doesn't have a `HostIP` field until Task 4, which revisits this exact function to add it.)

- [ ] **Step 7: Run the full package test + build**

Run: `go build ./... && go test ./internal/cli/ -v 2>&1 | tail -30`
Expected: clean build, no FAILs.

- [ ] **Step 8: Manual smoke-test**

Run:
```bash
go run ./cmd/mvm list --format table
go run ./cmd/mvm list --format json
go run ./cmd/mvm list --json
go run ./cmd/mvm list --format bogus   # expect a clear "invalid --format" error, non-zero exit
```
Expected: the first three produce output identical in shape to today's `list`/`list --json`; the last errors cleanly. If no VMs exist in this environment, "No microVMs..." (table) or `[]` (json) is fine — this is just confirming the flag plumbing, not real VM data.

- [ ] **Step 9: Commit**

```bash
git add internal/cli/format.go internal/cli/format_test.go internal/cli/list.go internal/cli/inspect.go
git commit -m "feat(cli): add --format json|table to mvm list and mvm inspect"
```

---

### Task 3: `--rm` on `mvm start` (rejected, not implemented)

**Files:**
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/start_test.go`

**Interfaces:**
- Produces: `func validateStartRM(rm bool) error`. Called once from `newStartCmd`'s `RunE`, before any of `runStart`'s existing logic runs.

**Decision (per the spec's Global Constraints and the design doc's decision #5 — "`start` is upsert, forever"):** `start` has no foreground lifetime to key deletion on — it returns as soon as the VM has booted, with no notion of "the command that was running." Silently reinterpreting `--rm` as "delete on `mvm stop`" would be a new, undocumented lifecycle rule bolted onto a command whose whole contract is idempotent, durable upsert; that's exactly the kind of divergence-by-accident the design spec's decision #5 explicitly rejects for `start`. **`mvm start --rm` is therefore a hard, immediate error directing the user to `mvm run`**, which already has the correct semantics: ephemeral (auto-deleted after its foreground command exits) unless `--name` is given. No new state field, no new deletion trigger, no interaction with `mvm stop`.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/start_test.go` (add `"strings"` to the import block):

```go
// === validateStartRM ===

func TestValidateStartRMRejectsFlag(t *testing.T) {
	err := validateStartRM(true)
	if err == nil {
		t.Fatal("validateStartRM(true) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "mvm run") {
		t.Errorf("error should point at mvm run, got: %v", err)
	}
}

func TestValidateStartRMAllowsDefault(t *testing.T) {
	if err := validateStartRM(false); err != nil {
		t.Errorf("validateStartRM(false) = %v, want nil", err)
	}
}
```

The resulting import block:

```go
package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestValidateStartRM -v`
Expected: FAIL — compile error `undefined: validateStartRM`.

- [ ] **Step 3: Write minimal implementation**

In `internal/cli/start.go`, add `validateStartRM` (near `parsePorts`, before `runStart`):

```go
// validateStartRM rejects --rm on `mvm start`. start has no foreground
// lifetime to key cleanup on (it returns right after boot, with no
// "the command that was running" to wait for) — silently reinterpreting
// --rm as "delete on `mvm stop`" would bolt an undocumented lifecycle rule
// onto a command whose whole contract is idempotent, durable upsert (see
// the design spec's decision #5: "start is upsert, forever"). mvm run
// already has the correct ephemeral-unless---name semantics, so this
// points there instead of inventing a second meaning for --rm.
func validateStartRM(rm bool) error {
	if rm {
		return fmt.Errorf("mvm start does not support --rm: start has no foreground command to key cleanup on. Use `mvm run <image>` instead — it deletes the VM automatically unless you pass --name")
	}
	return nil
}
```

Add the flag to `newStartCmd` and call the validator first in `RunE`:

```go
func newStartCmd(store *state.Store) *cobra.Command {
	var (
		detach    bool
		ports     []string
		netPolicy string
		volumes   []string
		seccomp   string
		watch     string
		cpus      int
		memoryMB  int
		image     string
		jsonOut   bool
		startup   string
		secretsF  []string
		rm        bool
	)

	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Create and boot a new microVM",
		Long: `Create and boot a new microVM.

  mvm start my-app
  mvm start my-app -p 8080:80           # forward host:8080 to guest:80
  mvm start my-app -p 3000:3000 -p 5432:5432
  mvm start my-app --net-policy deny     # block all outbound traffic
  mvm start my-app --net-policy allow:github.com,npmjs.org
  mvm start my-app -v ./src:/app         # mount host dir into guest
  mvm start my-app --seccomp strict      # restrict syscalls
  mvm start my-app --watch ./src         # rebuild on file changes
  mvm start my-app --cpus 4 --memory 2048  # custom resources
  mvm start my-app --image my-image       # use custom rootfs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateStartRM(rm); err != nil {
				return err
			}
			portMaps, err := parsePorts(ports)
			if err != nil {
				return err
			}
			var spec *StartupSpec
			if startup != "" {
				spec, err = loadStartupSpec(startup)
				if err != nil {
					return err
				}
			}
			return runStart(store, args[0], detach, portMaps, netPolicy, volumes, seccomp, watch, cpus, memoryMB, image, jsonOut, spec, secretsF)
		},
	}

	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "detach: don't stream boot output, return immediately after VM starts")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit a structured JSON result with boot path and per-phase timing")
	cmd.Flags().StringVar(&startup, "startup", "", "JSON startup recipe: git clone + commands + ready-check (applevz)")
	cmd.Flags().StringArrayVar(&secretsF, "secret", nil, "attach a stored secret, injected per-exec (repeatable; applevz)")
	cmd.Flags().StringArrayVarP(&ports, "publish", "p", nil, "publish port (hostPort:guestPort[/proto])")
	cmd.Flags().StringVar(&netPolicy, "net-policy", "open", "network policy: open, deny, or allow:domain1,domain2")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "V", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().StringVar(&seccomp, "seccomp", "", "seccomp profile: strict, moderate, or permissive")
	cmd.Flags().StringVar(&watch, "watch", "", "watch directory for changes and sync to guest")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVar(&memoryMB, "memory", 0, "RAM in MiB (default: 1024)")
	cmd.Flags().StringVar(&image, "image", "", "custom rootfs image name (built with mvm build)")
	cmd.Flags().BoolVar(&rm, "rm", false, "not supported on start — use mvm run instead")

	return cmd
}
```

`--rm` is registered (so `mvm start --help` documents it and `mvm start --rm` doesn't fail with cobra's "unknown flag") but is always rejected before `runStart` is ever called — `runStart`'s own signature and body are completely untouched.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestValidateStartRM -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Run the full package test + build**

Run: `go build ./... && go test ./internal/cli/ -v 2>&1 | tail -30`
Expected: clean build, no FAILs, `TestParsePorts`/`TestRunStart*`/etc. unaffected.

- [ ] **Step 6: Manual smoke-test**

Run: `go run ./cmd/mvm start --rm somename`
Expected: immediate error mentioning `mvm run`, no VM created (confirm with `go run ./cmd/mvm list` — `somename` must not appear).

- [ ] **Step 7: Commit**

```bash
git add internal/cli/start.go internal/cli/start_test.go
git commit -m "feat(cli): reject --rm on mvm start with a pointer to mvm run"
```

---

### Task 4: `[host-ip:]` prefix in `-p`

**Files:**
- Modify: `internal/state/store.go` (`PortMap`)
- Modify: `internal/cli/start.go` (`parsePorts`, `printPorts`, `runStartViaDaemon`)
- Modify: `internal/cli/start_test.go`
- Modify: `internal/firecracker/network.go` (`SetupPortForwarding`, `RemovePortForwarding`)
- Create: `internal/firecracker/network_hostip_test.go`
- Modify: `internal/preview/tunnel.go` (`Tunnel`)
- Modify: `internal/preview/tunnel_test.go`
- Modify: `internal/cli/forward_daemon.go` (`runForwardDaemon`)
- Modify: `internal/cli/inspect.go` (`printInspectTable`, from Task 2)
- Modify: `internal/server/schema_golden_test.go` (verification step only — see Grounded finding below)

**Interfaces:**
- `state.PortMap` gains `HostIP string \`json:"host_ip,omitempty"\`` (additive).
- `parsePorts` signature is unchanged (`func parsePorts(ports []string) ([]state.PortMap, error)`) — it now accepts the extended `[host-ip:]hostPort:guestPort[/proto]` grammar.
- `preview.Tunnel` gains a `BindIP string` field (additive; empty preserves its existing hardcoded `127.0.0.1` default).

**Grounded finding — the two backends' existing defaults are NOT symmetric, contradicting the prompt's premise that "default remains all-interfaces":**
- **applevz** (`internal/preview/tunnel.go`'s `Tunnel.Listen`): hardcodes `net.Listen("tcp", "127.0.0.1:%d")` today. The package doc comment is explicit about why: *"This is the safe, local-only tunnel (à la `kubectl port-forward`): it binds 127.0.0.1... No public listener."* This is a deliberate existing safety choice, not an oversight. This task must **not** flip that default to all-interfaces — doing so would be a silent security regression disguised as a compat feature. The default stays loopback-only; `-p 0.0.0.0:8080:80` becomes the explicit opt-in for "all interfaces," and `-p 8080:80` (no host-ip) keeps behaving exactly as it does today.
- **Firecracker/Lima** (`internal/firecracker/network.go`'s `SetupPortForwarding`): has no host-bind concept at all today — it only issues in-Lima iptables DNAT rules (`PREROUTING`/`OUTPUT`) rewriting the *destination* of packets already inside Lima's network namespace to `guestIP:guestPort`. It has no `-d` (destination-address) filter, so it matches traffic regardless of which interface inside Lima it arrived on. What macOS-host address a client actually dials to reach that traffic at all (`localhost:hostPort`, per `runStartViaDaemon`'s boot banner) is controlled by Lima's own networking layer (`internal/lima/lima.go`'s `CreateVM`, which only declares one static `portForwards` entry for the daemon's own control socket — no evidence in this codebase of Lima's automatic per-VM-port host-bind mechanism being independently configurable). **This plan cannot fully verify, from the code alone, that adding a `-d <host-ip>` filter to the DNAT rule changes what host address is actually reachable** — it changes which destination *address inside Lima's namespace* the rule matches, which is the correct, honest, additive thing to implement given what's traceable in this repo, but Step 8 below calls out manual verification against a real Lima VM as a required check before calling the Firecracker side of this task done, rather than asserting it works from static reading alone.

- [ ] **Step 1: Add `HostIP` to `state.PortMap`**

In `internal/state/store.go`, change:

```go
// PortMap represents a host:guest port forwarding rule.
type PortMap struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"` // "tcp" or "udp"
}
```

to:

```go
// PortMap represents a host:guest port forwarding rule.
type PortMap struct {
	HostIP    string `json:"host_ip,omitempty"` // bind address on the host; "" = the backend's existing default (see parsePorts)
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"` // "tcp" or "udp"
}
```

- [ ] **Step 2: Write the failing `parsePorts` tests**

In `internal/cli/start_test.go`, extend `TestParsePorts`'s table with a `wantHostIP` column (empty string for every existing row — this is the "additive, doesn't change existing behavior" assertion) and new host-ip rows, and add dedicated error-case tests:

```go
func TestParsePorts(t *testing.T) {
	tests := []struct {
		input      []string
		wantLen    int
		wantErr    bool
		wantHostIP string
		wantHost   int
		wantGuest  int
		wantProto  string
	}{
		{[]string{"8080:80"}, 1, false, "", 8080, 80, "tcp"},
		{[]string{"3000:3000"}, 1, false, "", 3000, 3000, "tcp"},
		{[]string{"53:53/udp"}, 1, false, "", 53, 53, "udp"},
		{[]string{"8080:80", "3000:3000"}, 2, false, "", 8080, 80, "tcp"},
		{nil, 0, false, "", 0, 0, ""},
		{[]string{}, 0, false, "", 0, 0, ""},
		{[]string{"invalid"}, 0, true, "", 0, 0, ""},
		{[]string{"abc:80"}, 0, true, "", 0, 0, ""},
		{[]string{"8080:abc"}, 0, true, "", 0, 0, ""},
		{[]string{"127.0.0.1:8080:80"}, 1, false, "127.0.0.1", 8080, 80, "tcp"},
		{[]string{"0.0.0.0:8080:80/udp"}, 1, false, "0.0.0.0", 8080, 80, "udp"},
		{[]string{"192.168.1.5:53:53"}, 1, false, "192.168.1.5", 53, 53, "tcp"},
		{[]string{":8080:80"}, 0, true, "", 0, 0, ""},          // empty host-ip
		{[]string{"1:2:3:4"}, 0, true, "", 0, 0, ""},           // too many segments
		{[]string{"127.0.0.1:abc:80"}, 0, true, "", 0, 0, ""},  // bad host port with host-ip present
	}

	for _, tt := range tests {
		ports, err := parsePorts(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parsePorts(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if len(ports) != tt.wantLen {
			t.Errorf("parsePorts(%v) len = %d, want %d", tt.input, len(ports), tt.wantLen)
			continue
		}
		if tt.wantLen > 0 {
			if ports[0].HostIP != tt.wantHostIP {
				t.Errorf("HostIP = %q, want %q", ports[0].HostIP, tt.wantHostIP)
			}
			if ports[0].HostPort != tt.wantHost {
				t.Errorf("HostPort = %d, want %d", ports[0].HostPort, tt.wantHost)
			}
			if ports[0].GuestPort != tt.wantGuest {
				t.Errorf("GuestPort = %d, want %d", ports[0].GuestPort, tt.wantGuest)
			}
			if ports[0].Proto != tt.wantProto {
				t.Errorf("Proto = %q, want %q", ports[0].Proto, tt.wantProto)
			}
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestParsePorts -v`
Expected: FAIL — compile error (`ports[0].HostIP` undefined until Step 1's struct change lands — Step 1 must be applied first for this to even compile; then it FAILs on the new host-ip rows since `parsePorts` doesn't parse them yet).

- [ ] **Step 4: Extend `parsePorts`**

In `internal/cli/start.go`, replace `parsePorts`:

```go
func parsePorts(ports []string) ([]state.PortMap, error) {
	var result []state.PortMap
	for _, p := range ports {
		proto := "tcp"
		if idx := strings.Index(p, "/"); idx != -1 {
			proto = p[idx+1:]
			p = p[:idx]
		}
		parts := strings.Split(p, ":")

		var hostIP, hostPortStr, guestPortStr string
		switch len(parts) {
		case 2:
			hostPortStr, guestPortStr = parts[0], parts[1]
		case 3:
			hostIP, hostPortStr, guestPortStr = parts[0], parts[1], parts[2]
			if hostIP == "" {
				return nil, fmt.Errorf("invalid port format %q (empty host-ip; use hostPort:guestPort to bind the backend's default address)", p)
			}
		default:
			return nil, fmt.Errorf("invalid port format %q (expected [host-ip:]hostPort:guestPort)", p)
		}

		host, err := strconv.Atoi(hostPortStr)
		if err != nil {
			return nil, fmt.Errorf("invalid host port %q: %w", hostPortStr, err)
		}
		guest, err := strconv.Atoi(guestPortStr)
		if err != nil {
			return nil, fmt.Errorf("invalid guest port %q: %w", guestPortStr, err)
		}
		result = append(result, state.PortMap{HostIP: hostIP, HostPort: host, GuestPort: guest, Proto: proto})
	}
	return result, nil
}
```

Note: IPv6 host-ip literals (which contain colons themselves) are not handled — `strings.Split(p, ":")` would produce more than 3 segments and hit the `default:` error case. This matches the "cheap, honest v1" scope; bracketed-IPv6 support (`[::1]:8080:80`) is a documented follow-up, not silently broken (it errors clearly rather than mis-parsing).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestParsePorts -v`
Expected: PASS (all rows, including the new host-ip and error cases).

- [ ] **Step 6: Thread `HostIP` through the Firecracker/Lima iptables path**

In `internal/firecracker/network.go`, replace `SetupPortForwarding` and `RemovePortForwarding`:

```go
// SetupPortForwarding creates iptables DNAT rules in Lima to forward
// host ports to guest ports inside the microVM. A non-empty p.HostIP adds a
// destination-address filter (-d) to each rule, restricting it to traffic
// addressed to that IP inside Lima's own network namespace; empty (the
// default) matches any destination, unchanged from prior behavior.
func SetupPortForwarding(exec Executor, vm *state.VM) error {
	if len(vm.Ports) == 0 {
		return nil
	}

	var rules []string
	for _, p := range vm.Ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		destFilter := ""
		if p.HostIP != "" {
			destFilter = fmt.Sprintf(" -d %s", p.HostIP)
		}
		// DNAT: traffic to Lima's localhost:hostPort -> guest:guestPort
		rules = append(rules,
			fmt.Sprintf("sudo iptables -t nat -A PREROUTING -p %s%s --dport %d -j DNAT --to-destination %s:%d",
				proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort),
			// Also handle locally-originated traffic
			fmt.Sprintf("sudo iptables -t nat -A OUTPUT -p %s%s --dport %d -j DNAT --to-destination %s:%d",
				proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort),
		)
	}

	cmd := strings.Join(rules, " && ")
	_, err := exec.Run(cmd)
	if err != nil {
		return fmt.Errorf("setup port forwarding: %w", err)
	}
	return nil
}

// RemovePortForwarding removes iptables DNAT rules for a VM. The -d filter
// (if any) must match exactly what SetupPortForwarding added, or the
// deletion silently won't find the rule.
func RemovePortForwarding(exec Executor, vm *state.VM) {
	for _, p := range vm.Ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		destFilter := ""
		if p.HostIP != "" {
			destFilter = fmt.Sprintf(" -d %s", p.HostIP)
		}
		exec.Run(fmt.Sprintf(
			"sudo iptables -t nat -D PREROUTING -p %s%s --dport %d -j DNAT --to-destination %s:%d 2>/dev/null; "+
				"sudo iptables -t nat -D OUTPUT -p %s%s --dport %d -j DNAT --to-destination %s:%d 2>/dev/null",
			proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort,
			proto, destFilter, p.HostPort, vm.GuestIP, p.GuestPort,
		))
	}
}
```

- [ ] **Step 7: Write and pass `network_hostip_test.go`**

Create `internal/firecracker/network_hostip_test.go`:

```go
package firecracker

import (
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

type recordingExecutor struct {
	commands []string
}

func (r *recordingExecutor) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	return "", nil
}
func (r *recordingExecutor) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	return r.Run(command)
}

func TestSetupPortForwardingOmitsDestFilterWhenHostIPEmpty(t *testing.T) {
	ex := &recordingExecutor{}
	vm := &state.VM{GuestIP: "172.16.0.2", Ports: []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}}}
	if err := SetupPortForwarding(ex, vm); err != nil {
		t.Fatalf("SetupPortForwarding: %v", err)
	}
	if len(ex.commands) != 1 {
		t.Fatalf("expected 1 command, got %d: %v", len(ex.commands), ex.commands)
	}
	if strings.Contains(ex.commands[0], " -d ") {
		t.Errorf("command should have no -d filter when HostIP is empty: %q", ex.commands[0])
	}
}

func TestSetupPortForwardingAddsDestFilterWhenHostIPSet(t *testing.T) {
	ex := &recordingExecutor{}
	vm := &state.VM{GuestIP: "172.16.0.2", Ports: []state.PortMap{{HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80, Proto: "tcp"}}}
	if err := SetupPortForwarding(ex, vm); err != nil {
		t.Fatalf("SetupPortForwarding: %v", err)
	}
	if !strings.Contains(ex.commands[0], "-d 127.0.0.1") {
		t.Errorf("command should filter on -d 127.0.0.1: %q", ex.commands[0])
	}
	// Both PREROUTING and OUTPUT rules get the filter.
	if strings.Count(ex.commands[0], "-d 127.0.0.1") != 2 {
		t.Errorf("expected -d filter on both PREROUTING and OUTPUT rules: %q", ex.commands[0])
	}
}

func TestRemovePortForwardingMatchesHostIPFilter(t *testing.T) {
	ex := &recordingExecutor{}
	vm := &state.VM{GuestIP: "172.16.0.2", Ports: []state.PortMap{{HostIP: "192.168.1.5", HostPort: 53, GuestPort: 53, Proto: "udp"}}}
	RemovePortForwarding(ex, vm)
	if len(ex.commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(ex.commands))
	}
	if !strings.Contains(ex.commands[0], "-d 192.168.1.5") {
		t.Errorf("delete command should match the same -d filter that was added: %q", ex.commands[0])
	}
}
```

Run: `go test ./internal/firecracker/ -run 'TestSetupPortForwarding|TestRemovePortForwarding' -v`
Expected: PASS (3 tests).

- [ ] **Step 8: Thread `HostIP` through the applevz forwarder**

In `internal/preview/tunnel.go`, add a `BindIP` field to `Tunnel` and use it in `Listen`, preserving the existing loopback-only default:

```go
// Tunnel forwards a local loopback port to a guest port.
type Tunnel struct {
	GuestPort int
	Dial      GuestDial

	// BindIP is the host address to bind. Empty defaults to "127.0.0.1" —
	// this package's documented safe, local-only default (see the package
	// doc comment). Set explicitly (e.g. "0.0.0.0") to opt into a wider
	// bind; this plan never changes the default itself.
	BindIP string

	ln net.Listener
}

// Listen binds BindIP:localPort (BindIP empty = "127.0.0.1"; localPort 0
// picks a free port) and returns the bound address. It does not accept
// connections until Serve is called.
func (t *Tunnel) Listen(localPort int) (string, error) {
	bindIP := t.BindIP
	if bindIP == "" {
		bindIP = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindIP, localPort))
	if err != nil {
		return "", err
	}
	t.ln = ln
	return ln.Addr().String(), nil
}
```

Append to `internal/preview/tunnel_test.go` (read the existing file first to match its style and avoid name collisions before appending):

```go
func TestTunnelListenDefaultsToLoopback(t *testing.T) {
	tun := &Tunnel{GuestPort: 80}
	addr, err := tun.Listen(0)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer tun.ln.Close()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("Listen() addr = %q, want a 127.0.0.1 default (unchanged from before BindIP existed)", addr)
	}
}

func TestTunnelListenHonorsExplicitBindIP(t *testing.T) {
	tun := &Tunnel{GuestPort: 80, BindIP: "127.0.0.1"}
	addr, err := tun.Listen(0)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer tun.ln.Close()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("Listen() addr = %q, want 127.0.0.1", addr)
	}
}
```

(`"strings"` must be present in `tunnel_test.go`'s import block — add it if the existing file doesn't already import it.)

Run: `go test ./internal/preview/ -v`
Expected: PASS, including pre-existing tests.

- [ ] **Step 9: Wire `HostIP` into `runForwardDaemon`**

In `internal/cli/forward_daemon.go`'s `runForwardDaemon`, pass `p.HostIP` through to the `Tunnel`:

```go
		tun := &preview.Tunnel{
			GuestPort: guestPort,
			BindIP:    p.HostIP,
			Dial: func(ctx context.Context, port int) (net.Conn, error) {
				return agent.Forward(ctx, port)
			},
		}
```

(One-line change — `p` is already in scope as the loop variable over `vm.Ports`.)

- [ ] **Step 10: Update the two human boot-banner port lines and `printInspectTable`**

In `internal/cli/start.go`, update `printPorts` and the port-print loop in `runStartViaDaemon` to show the bind address when set:

```go
func printPorts(vm *state.VM) {
	for _, p := range vm.Ports {
		host := p.HostIP
		if host == "" {
			host = "localhost"
		}
		fmt.Printf("    Port: %s:%d -> %s:%d/%s\n", host, p.HostPort, vm.GuestIP, p.GuestPort, p.Proto)
	}
}
```

```go
	for _, p := range resp.Ports {
		host := p.HostIP
		if host == "" {
			host = "localhost"
		}
		fmt.Printf("    Port: %s:%d -> %s:%d/%s\n", host, p.HostPort, resp.GuestIP, p.GuestPort, p.Proto)
	}
```

(this second block is inside `runStartViaDaemon`, replacing its existing `fmt.Printf("    Port: localhost:%d -> %s:%d/%s\n", ...)` loop.)

In `internal/cli/inspect.go`'s `printInspectTable` (added in Task 2), update the `Port:` line the same way:

```go
	for _, p := range resp.Ports {
		host := p.HostIP
		if host == "" {
			host = "localhost"
		}
		fmt.Fprintf(w, "Port:\t%s:%d -> %d/%s\n", host, p.HostPort, p.GuestPort, p.Proto)
	}
```

- [ ] **Step 11: Verify the JSON schema goldens are unaffected**

Run: `go test ./internal/server/ -run TestVM -v`
Expected: PASS, unmodified — confirms in practice the Global Constraints finding that `PortMap.HostIP` doesn't change `VMResponse`/`VMInspectResponse`/`VMSpec`'s top-level key sets. Do not edit `schema_golden_test.go` in this task; if this step unexpectedly fails, stop and re-read `jsonKeys`'s implementation before touching the golden `want` slices — that would mean the grounded finding above was wrong somewhere and needs re-diagnosis, not a golden update to "make the failure go away."

- [ ] **Step 12: Full build + test**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: clean build, `go vet` silent, no FAILs.

- [ ] **Step 13: Manual verification (required — see the Grounded finding above)**

On a host with mvm initialized (`firecracker` backend) and Lima running:
```bash
go run ./cmd/mvm start hosttest -p 127.0.0.1:19999:80
curl -sf http://127.0.0.1:19999/ || echo "unreachable from 127.0.0.1 as expected/unexpected — record which"
go run ./cmd/mvm delete hosttest --force
```
Expected/required observation: confirm empirically whether the `-d 127.0.0.1` iptables filter actually changes host-reachability on this machine's Lima networking setup, and note the result in the commit message or a follow-up issue — this plan implements the traceable, additive half (the DNAT filter) honestly but cannot prove end-to-end host-bind behavior from static code reading alone (see the Grounded finding above `Step 1`). If this environment has no Firecracker/Lima backend available, skip this step and note it explicitly in the completion report rather than silently skipping — it is not a blocker for the unit-tested portions of this task, but it is a known open question that should not be silently dropped.

For applevz, the equivalent check (`Tunnel.BindIP`) is fully verifiable in code (Step 8's tests already do this — `net.Listen` binding is directly observable, unlike Lima's opaque host-forwarding layer), so no additional manual step is required for that backend.

- [ ] **Step 14: Commit**

```bash
git add internal/state/store.go internal/cli/start.go internal/cli/start_test.go \
  internal/firecracker/network.go internal/firecracker/network_hostip_test.go \
  internal/preview/tunnel.go internal/preview/tunnel_test.go \
  internal/cli/forward_daemon.go internal/cli/inspect.go
git commit -m "feat: add [host-ip:] prefix to -p port publishing"
```

---

### Task 5: `-d`/`--detach` on `mvm exec`

**Files:**
- Modify: `internal/cli/exec.go`
- Modify: `internal/cli/exec_test.go`

**Interfaces:**
- Produces: `func buildDetachedExecScript(remoteArgs []string, workdir string, envVars []string, user string) string`, `func validateExecFlags(detach, interactive, tty bool) error`.
- Changes signatures (internal, not exported outside the package, and every call site is updated in this same task): `runExec` and `runExecAppleVZ` each gain a `detach bool` parameter.

**Grounded finding — the guest agent has no detached-exec support at all**, confirmed by reading:
- `agent/internal/handler/exec.go`'s `HandleExec`: builds `exec.Command("sh", "-c", req.Command)`, then calls `cmd.Run()` — a **blocking** call that only returns once the child (and everything it spawned) exits, buffering all stdout/stderr into memory first.
- `agent/internal/protocol` / `internal/agentclient/client.go`'s `Exec`/`ExecResult`: the wire request has no `Background`/`Detach` field, and the response has no "started, PID X" shape — only `{Output, ExitCode}` after full completion.

A real detached-exec primitive (agent reports "started" immediately, exposes a PID, lets a later call fetch output/exit code) would require a new agent protocol request type — out of scope here. Per the prompt, **the cheap, honest v1 is a shell-level background wrapper**: build the exact same command `buildExecScript` already constructs, then wrap it with `setsid ... </dev/null >/dev/null 2>&1 &` so the *guest's* `sh -c` (which `HandleExec` blocks on) returns almost immediately once the background job is launched, detached into its own session so it survives past that point. This has real, documented limits: no output is ever returned (redirected to `/dev/null` — matches `docker exec -d`'s own documented behavior of discarding output), and there is no way to check on the job's later exit code or kill it by name (a known gap, not silently pretended away).

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/exec_test.go` (its only current import is `"testing"` — add `"strings"` to the import block too):

```go
// === buildDetachedExecScript ===

func TestBuildDetachedExecScriptWrapsWithSetsid(t *testing.T) {
	got := buildDetachedExecScript([]string{"sleep", "100"}, "", nil, "")
	if !strings.HasPrefix(got, "setsid sh -c ") {
		t.Errorf("buildDetachedExecScript() = %q, want prefix %q", got, "setsid sh -c ")
	}
	if !strings.HasSuffix(got, "</dev/null >/dev/null 2>&1 &") {
		t.Errorf("buildDetachedExecScript() = %q, want a fully-detached background suffix", got)
	}
}

func TestBuildDetachedExecScriptEmbedsInnerCommand(t *testing.T) {
	inner := buildExecScript([]string{"echo", "hi"}, "", nil, "")
	got := buildDetachedExecScript([]string{"echo", "hi"}, "", nil, "")
	if !strings.Contains(got, shellQuote(inner)) {
		t.Errorf("buildDetachedExecScript() = %q, want it to shell-quote and embed %q", got, inner)
	}
}

func TestBuildDetachedExecScriptRespectsWorkdirEnvUser(t *testing.T) {
	inner := buildExecScript([]string{"env"}, "/app", []string{"FOO=bar"}, "nobody")
	got := buildDetachedExecScript([]string{"env"}, "/app", []string{"FOO=bar"}, "nobody")
	if !strings.Contains(got, shellQuote(inner)) {
		t.Errorf("buildDetachedExecScript() = %q, want the same inner construction as buildExecScript, just wrapped", got)
	}
}

// === validateExecFlags ===

func TestValidateExecFlagsRejectsDetachWithInteractive(t *testing.T) {
	if err := validateExecFlags(true, true, false); err == nil {
		t.Fatal("want error combining --detach and --interactive")
	}
	if err := validateExecFlags(true, false, true); err == nil {
		t.Fatal("want error combining --detach and --tty")
	}
}

func TestValidateExecFlagsAllowsDetachAlone(t *testing.T) {
	if err := validateExecFlags(true, false, false); err != nil {
		t.Errorf("validateExecFlags(true, false, false) = %v, want nil", err)
	}
}

func TestValidateExecFlagsAllowsInteractiveWithoutDetach(t *testing.T) {
	if err := validateExecFlags(false, true, true); err != nil {
		t.Errorf("validateExecFlags(false, true, true) = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestBuildDetachedExecScript|TestValidateExecFlags' -v`
Expected: FAIL — compile errors `undefined: buildDetachedExecScript`, `undefined: validateExecFlags`.

- [ ] **Step 3: Write minimal implementation**

In `internal/cli/exec.go`, add both functions near `buildExecScript`:

```go
// validateExecFlags rejects combining --detach with --interactive/--tty:
// a detached job never gets a terminal or a stdin stream wired to it (see
// buildDetachedExecScript), so those flags would silently do nothing —
// better to fail fast than pretend they're honored.
func validateExecFlags(detach, interactive, tty bool) error {
	if detach && (interactive || tty) {
		return fmt.Errorf("--detach cannot be combined with --interactive/--tty")
	}
	return nil
}

// buildDetachedExecScript wraps the same command construction as
// buildExecScript but runs it fully backgrounded and detached inside the
// guest, for -d/--detach.
//
// This is the cheap, honest v1: the guest agent's wire protocol
// (agent/internal/protocol, internal/agentclient) has no notion of a
// background job — HandleExec (agent/internal/handler/exec.go) always runs
// "sh -c <command>" synchronously via cmd.Run() and returns combined
// stdout+stderr plus an exit code only once the whole thing finishes. There
// is no way today to report a PID back to the host, or to later fetch a
// backgrounded job's output/exit code — that needs an agent protocol change
// (a new request type, or an Exec.Background field plus a follow-up
// "collect" verb) and is out of scope here.
//
// setsid detaches the job into its own session so it survives the wrapping
// "sh -c" process exiting once it backgrounds the real work; </dev/null and
// >/dev/null 2>&1 close off stdio so nothing blocks HandleExec's cmd.Run()
// waiting on a pipe, and so no output is ever captured — matching `docker
// exec -d`'s own documented behavior of discarding output.
func buildDetachedExecScript(remoteArgs []string, workdir string, envVars []string, user string) string {
	inner := buildExecScript(remoteArgs, workdir, envVars, user)
	return fmt.Sprintf("setsid sh -c %s </dev/null >/dev/null 2>&1 &", shellQuote(inner))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestBuildDetachedExecScript|TestValidateExecFlags' -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Thread `detach` through `runExec`/`runExecAppleVZ` and `newExecCmd`**

Replace `internal/cli/exec.go`'s `newExecCmd` and `runExec`/`runExecAppleVZ` signatures and bodies:

```go
func newExecCmd(store *state.Store) *cobra.Command {
	var (
		interactive bool
		tty         bool
		detach      bool
		workdir     string
		envVars     []string
		envFile     string
		user        string
	)

	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a command in a running microVM",
		Long: `Run a command inside a running microVM.

  mvm exec my-vm -- ls /
  mvm exec my-vm -it -- bash
  mvm exec my-vm -e FOO=bar -- env
  mvm exec my-vm --env-file .env -- env
  mvm exec my-vm -d -- long-running-task    # detach: don't wait, no output
  echo "data" | mvm exec my-vm -- cat`,
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

	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run the command in the background; don't wait, output is discarded")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the VM")
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil, "set environment variables (KEY=VALUE)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read environment variables from a file (KEY=VALUE per line, # comments and blank lines skipped)")
	cmd.Flags().StringVarP(&user, "user", "u", "", "run as user")

	return cmd
}

func runExec(store *state.Store, name string, remoteArgs []string, interactive, detach bool, workdir string, envVars []string, user string) error {
	// Apple VZ VMs aren't managed by the daemon — exec directly against the
	// per-VM mvm-vz helper's vsock-bridged agent.
	if vm, _ := store.GetVM(name); vm != nil && vm.Backend == "applevz" {
		return runExecAppleVZ(store, vm, remoteArgs, interactive, detach, workdir, envVars, user)
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	if detach {
		script := buildDetachedExecScript(remoteArgs, workdir, envVars, user)
		ctx := context.Background()
		_, exitCode, err := sc.Exec(ctx, name, script)
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return fmt.Errorf("failed to launch detached command (exit code %d)", exitCode)
		}
		return nil
	}

	script := buildExecScript(remoteArgs, workdir, envVars, user)

	if interactive {
		// Put the terminal in raw mode so keystrokes are forwarded
		// directly to the guest PTY without local echo or line buffering.
		fd := int(os.Stdin.Fd())
		if term.IsTerminal(fd) {
			oldState, err := term.MakeRaw(fd)
			if err != nil {
				return fmt.Errorf("failed to set raw terminal: %w", err)
			}
			defer term.Restore(fd, oldState)
		}

		ctx := context.Background()
		exitCode, err := sc.ExecInteractive(ctx, name, script, os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return fmt.Errorf("exit code %d", exitCode)
		}
		return nil
	}

	ctx := context.Background()
	exitCode, err := sc.ExecStream(ctx, name, script, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("exit code %d", exitCode)
	}
	return nil
}
```

And `runExecAppleVZ`:

```go
// runExecAppleVZ runs a command on an Apple VZ VM via the per-VM mvm-vz
// helper's vsock-bridged agent (no daemon). MVP: non-interactive only.
func runExecAppleVZ(store *state.Store, vm *state.VM, remoteArgs []string, interactive, detach bool, workdir string, envVars []string, user string) error {
	if vm.Status != "running" && vm.Status != "paused" {
		return fmt.Errorf("microVM %q is not running (status: %s)", vm.Name, vm.Status)
	}
	if interactive {
		return runExecAppleVZInteractive(store, vm, remoteArgs, workdir, envVars, user)
	}

	// Resume-on-exec: wake a paused VM before running, then record activity
	// so the idle checker doesn't immediately re-pause it.
	AutoResumeIfPaused(nil, store, vm)
	TouchActivity(store, vm.Name)

	// Inject the VM's attached secrets, decrypted from host memory at call time.
	// They go in as env exports and are never written to a guest file.
	if len(vm.Secrets) > 0 {
		secretEnv, err := secretEnvVars(vm.Secrets)
		if err != nil {
			return fmt.Errorf("load secrets for %q: %w", vm.Name, err)
		}
		envVars = append(envVars, secretEnv...)
	}

	if detach {
		script := buildDetachedExecScript(remoteArgs, workdir, envVars, user)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := vm_pkg.NewAppleVZBackend(mvmDir).AgentClient(vm.Name).Exec(ctx, script, "")
		if err != nil {
			return fmt.Errorf("exec on %q: %w", vm.Name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("failed to launch detached command (exit code %d)", res.ExitCode)
		}
		return nil
	}

	// Forward piped/redirected stdin (echo data | mvm exec vm -- cat, or
	// mvm exec vm -- cat < file). Only attempt this when stdin is not an
	// interactive tty, and bound the read: an inherited fd that never reaches
	// EOF (a held-open pipe under CI/cron, or /dev/null variants) must not be
	// able to hang exec. A real pipe/file delivers its data + EOF promptly, so
	// the select returns as soon as the data is in — the timeout only fires for
	// a source that is never going to send anything.
	stdin := readStdinNonBlocking()

	script := buildExecScript(remoteArgs, workdir, envVars, user)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := vm_pkg.NewAppleVZBackend(mvmDir).AgentClient(vm.Name).Exec(ctx, script, stdin)
	if err != nil {
		return fmt.Errorf("exec on %q: %w", vm.Name, err)
	}
	if res.Output != "" {
		os.Stdout.WriteString(res.Output)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit code %d", res.ExitCode)
	}
	return nil
}
```

(Moved the secrets-injection block above the `detach` branch so a detached command on applevz still gets its attached secrets — it was previously positioned after the point where `detach` now branches off.)

- [ ] **Step 6: Update `runExec`'s other call site**

`internal/cli/run.go`'s `runRun` calls `runExec` twice (the readiness probe and the real foreground command) — both are always foreground, so both calls get an explicit `false` for the new `detach` parameter:

```go
	if err := waitForReady(30*time.Second, func() error {
		return runExec(store, name, []string{"true"}, false, false, "", nil, "")
	}); err != nil {
```

```go
	execErr := runExec(store, name, cmdArgs, interactive, false, workdir, envVars, user)
```

- [ ] **Step 7: Run the full package test + build**

Run: `go build ./... && go test ./internal/cli/ -v 2>&1 | tail -40`
Expected: clean build, no FAILs — in particular `TestRunRunRejectsCustomImageOnAppleVZ` (which calls `runRun`, not `runExec`, directly, so it's unaffected) and every existing `exec_test.go` test still pass.

- [ ] **Step 8: Manual smoke-test**

If a backend is available:
```bash
go run ./cmd/mvm start dtest
go run ./cmd/mvm exec dtest -d -- sh -c 'sleep 5; touch /tmp/detach-worked'
go run ./cmd/mvm exec dtest -- sleep 6   # give the detached job time to finish
go run ./cmd/mvm exec dtest -- test -f /tmp/detach-worked && echo "detach confirmed"
go run ./cmd/mvm exec dtest -d -i -- bash   # expect an immediate --detach/--interactive conflict error
go run ./cmd/mvm delete dtest --force
```
Expected: `mvm exec -d` returns almost immediately (no 5s wait), the file exists after the sleep completes, and the `-d -i` combination errors before any daemon call. If no backend is available in this environment, skip and note it — not a blocker for the unit-tested portions.

- [ ] **Step 9: Commit**

```bash
git add internal/cli/exec.go internal/cli/exec_test.go internal/cli/run.go
git commit -m "feat(cli): add -d/--detach to mvm exec"
```

---

### Task 6: `mvm stats`

**Files:**
- Create: `internal/firecracker/stats.go`
- Test: `internal/firecracker/stats_test.go`
- Modify: `internal/server/routes.go` (`VMStats`, `handleStatsVMs`)
- Modify: `internal/server/server.go` (route registration)
- Modify: `internal/server/client.go` (`StatsVMs`)
- Test: `internal/server/schema_golden_test.go` (new golden for `VMStats`)
- Create: `internal/cli/stats.go`
- Test: `internal/cli/stats_test.go`
- Modify: `internal/cli/root.go` (register `newStatsCmd`)

**Interfaces:**
- Produces: `func firecracker.ParsePSOutput(out string) (cpuPct, memMB float64, err error)`, `func firecracker.ProcessStats(ex Executor, pid int) (cpuPct, memMB float64, err error)`.
- Produces (server): `type server.VMStats struct{...}`, `func (s *Server) handleStatsVMs(w http.ResponseWriter, r *http.Request)`, `func (c *Client) StatsVMs(ctx context.Context) ([]VMStats, error)`.
- Produces (cli): `func hostProcessStats(pid int) (cpuPct, memMB float64, err error)`, `func newStatsCmd(store *state.Store) *cobra.Command`, `func runStats(store *state.Store, names []string, wantJSON bool) error`.
- Consumes: `resolveFormat` (Task 2), `localApplevzVMs` (`internal/cli/list.go`, existing), `requireDaemon` (`internal/cli/helpers.go`, existing), `state.VM.PID` (existing field — for applevz this is the mvm-vz helper's host PID per `runStartAppleVZ`'s `pid := startResult.PID`; for Firecracker this is the Firecracker binary's PID *inside Lima's process namespace*, per `internal/firecracker/process.go`'s `parsePID` reading the daemon start script's `PID:$FC_PID` line run through the `Executor`).

**Grounded scope decision (v1, per the prompt's explicit allowance):** point-in-time only, mirroring `docker stats --no-stream`. No continuous streaming — `--no-stream` is accepted as a flag (forward-compatible, matches the eventual streaming mode's expected flag surface) but is currently the *only* supported behavior; omitting it behaves identically. Streaming is out of scope, tracked as a follow-up in this task's own doc comments.

- [ ] **Step 1: Write the failing `ParsePSOutput`/`ProcessStats` tests**

Create `internal/firecracker/stats_test.go`:

```go
package firecracker

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// === ParsePSOutput ===

func TestParsePSOutputParsesCPUAndRSS(t *testing.T) {
	cpu, memMB, err := ParsePSOutput(" 12.5 204800\n")
	if err != nil {
		t.Fatalf("ParsePSOutput: %v", err)
	}
	if cpu != 12.5 {
		t.Errorf("cpu = %v, want 12.5", cpu)
	}
	if memMB != 200 { // 204800 KiB / 1024 = 200 MiB
		t.Errorf("memMB = %v, want 200", memMB)
	}
}

func TestParsePSOutputRejectsMalformed(t *testing.T) {
	if _, _, err := ParsePSOutput("garbage"); err == nil {
		t.Fatal("ParsePSOutput(\"garbage\") = nil error, want error")
	}
	if _, _, err := ParsePSOutput(""); err == nil {
		t.Fatal("ParsePSOutput(\"\") = nil error, want error")
	}
	if _, _, err := ParsePSOutput("abc 123"); err == nil {
		t.Fatal("ParsePSOutput(\"abc 123\") = nil error, want error (bad cpu field)")
	}
}

// === ProcessStats ===

type stubStatsExecutor struct {
	out string
	err error
}

func (s *stubStatsExecutor) Run(command string) (string, error) { return s.out, s.err }
func (s *stubStatsExecutor) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	return s.out, s.err
}

func TestProcessStatsRunsPsWithPID(t *testing.T) {
	ex := &stubStatsExecutor{out: "3.0 51200\n"}
	cpu, memMB, err := ProcessStats(ex, 4242)
	if err != nil {
		t.Fatalf("ProcessStats: %v", err)
	}
	if cpu != 3.0 || memMB != 50 {
		t.Errorf("ProcessStats() = %v, %v, want 3.0, 50", cpu, memMB)
	}
}

func TestProcessStatsPropagatesExecutorError(t *testing.T) {
	ex := &stubStatsExecutor{err: fmt.Errorf("no such process")}
	if _, _, err := ProcessStats(ex, 1); err == nil {
		t.Fatal("ProcessStats() = nil error, want the executor's error propagated")
	} else if !strings.Contains(err.Error(), "no such process") {
		t.Errorf("error should wrap the underlying cause, got: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/firecracker/ -run 'TestParsePSOutput|TestProcessStats' -v`
Expected: FAIL — compile errors `undefined: ParsePSOutput`, `undefined: ProcessStats`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/firecracker/stats.go`:

```go
package firecracker

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePSOutput parses the output of `ps -o %cpu=,rss= -p <pid>` (two
// whitespace-separated numeric fields, no header — the trailing "=" on each
// -o key suppresses ps's column header). Shared by ProcessStats (Firecracker,
// run inside Lima via an Executor) and the CLI's own host-local ps call for
// applevz PIDs (internal/cli/stats.go's hostProcessStats), so the parsing
// logic — and its test coverage — isn't duplicated between the two
// transports.
func ParsePSOutput(out string) (cpuPct, memMB float64, err error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ps output %q (want 2 fields: %%cpu rss)", out)
	}
	cpuPct, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %%cpu %q: %w", fields[0], err)
	}
	rssKB, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse rss %q: %w", fields[1], err)
	}
	return cpuPct, rssKB / 1024.0, nil
}

// ProcessStats reports point-in-time CPU% and resident memory (MiB) for pid,
// which must be a PID inside ex's namespace — for the Firecracker backend
// that's Lima's process namespace, not the macOS host's (see process.go's
// parsePID / Start, which reports the Firecracker binary's PID as observed
// by the same Executor). This is a snapshot, not a rate: %cpu as ps reports
// it is a lifetime-average, not "usage over the last second" — a true
// live/streaming view would need to poll and diff /proc/<pid>/stat, tracked
// as a follow-up (see docs/superpowers/plans/2026-07-19-container-ergonomics.md Task 6).
func ProcessStats(ex Executor, pid int) (cpuPct, memMB float64, err error) {
	out, err := ex.Run(fmt.Sprintf("ps -o %%cpu=,rss= -p %d", pid))
	if err != nil {
		return 0, 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return ParsePSOutput(out)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/firecracker/ -run 'TestParsePSOutput|TestProcessStats' -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Add `VMStats` and `handleStatsVMs` to the server**

In `internal/server/routes.go`, add near `VMResponse`:

```go
// VMStats is a point-in-time resource-usage snapshot for one VM (v1: no
// streaming — see handleStatsVMs). Backend split mirrors VMResponse: the
// daemon only ever reports Firecracker VMs (Error is set, not omitted-and-
// silent, when a running VM's stats couldn't be read, e.g. between "marked
// running" and the process actually being observable).
type VMStats struct {
	Name    string  `json:"name"`
	Backend string  `json:"backend,omitempty"`
	PID     int     `json:"pid,omitempty"`
	CPUPct  float64 `json:"cpu_pct"`
	MemMB   float64 `json:"mem_mb"`
	Status  string  `json:"status"`
	Error   string  `json:"error,omitempty"`
}
```

and a handler near `handleListVMs`:

```go
// handleStatsVMs reports point-in-time CPU/memory for every Firecracker VM
// the daemon knows about. applevz VMs are never included here — the daemon
// has never heard of them (same split as handleListVMs's CLI-side caller,
// internal/cli/list.go's localApplevzVMs); the CLI's own mvm stats command
// merges those in separately via a direct host-local ps call, since the
// applevz mvm-vz helper's PID lives on the macOS host, not inside Lima.
func (s *Server) handleStatsVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.store.ListVMs()
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	result := make([]VMStats, 0, len(vms))
	for _, vm := range vms {
		if vm.Backend == "applevz" {
			continue
		}
		st := VMStats{Name: vm.Name, Backend: vm.Backend, PID: vm.PID, Status: vm.Status}
		if vm.Status == "running" && vm.PID > 0 {
			cpu, memMB, err := firecracker.ProcessStats(s.executor, vm.PID)
			if err != nil {
				st.Error = err.Error()
			} else {
				st.CPUPct, st.MemMB = cpu, memMB
			}
		}
		result = append(result, st)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
```

In `internal/server/server.go`'s `buildMux`, register the route right after `register("GET", "/vms", s.handleListVMs)`:

```go
	register("GET", "/vms", s.handleListVMs)
	register("GET", "/vms/stats", s.handleStatsVMs)
	register("GET", "/vms/{name}", s.handleInspectVM)
```

(Registered before the `/vms/{name}` wildcard route is only for readability — Go 1.22's `net/http.ServeMux` already prefers a literal segment over a wildcard for the same position regardless of registration order, so `GET /vms/stats` correctly never falls through to the `{name}` handler with `name="stats"`; verified against the stdlib's documented pattern-specificity rule.)

- [ ] **Step 6: Add `StatsVMs` to `server.Client`**

In `internal/server/client.go`, add near `ListVMs`:

```go
// StatsVMs returns point-in-time CPU/memory stats for every Firecracker VM
// the daemon manages (v1: no streaming).
func (c *Client) StatsVMs(ctx context.Context) ([]VMStats, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url("/vms/stats"), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	var result []VMStats
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}
```

- [ ] **Step 7: Add a golden test for `VMStats`**

This is a wholly new top-level JSON schema (not a field added to an existing one), so per the repo's convention it gets its own golden, appended to `internal/server/schema_golden_test.go`:

```go
func TestVMStatsSchemaGolden(t *testing.T) {
	full := VMStats{
		Name:    "vm",
		Backend: "firecracker",
		PID:     1,
		CPUPct:  1.5,
		MemMB:   256,
		Status:  "running",
		Error:   "e",
	}
	want := []string{"backend", "cpu_pct", "error", "mem_mb", "name", "pid", "status"}
	if got := jsonKeys(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("VMStats keys = %v, want %v (additive-only: update want when adding; never remove/rename)", got, want)
	}
}
```

- [ ] **Step 8: Run the server package tests**

Run: `go build ./... && go test ./internal/server/ -v 2>&1 | tail -30`
Expected: clean build, all PASS, including the pre-existing `TestVMResponseSchemaGolden`/`TestVMInspectResponseSchemaGolden` (unaffected) and the new `TestVMStatsSchemaGolden`.

- [ ] **Step 9: Add `mvm stats` to the CLI**

Create `internal/cli/stats.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"text/tabwriter"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newStatsCmd(store *state.Store) *cobra.Command {
	var (
		format   string
		noStream bool
	)

	cmd := &cobra.Command{
		Use:   "stats [name...]",
		Short: "Show live resource usage for running microVMs",
		Long: `Show a point-in-time snapshot of CPU and memory usage for running microVMs.

  mvm stats                  # all running VMs
  mvm stats my-vm             # a single VM
  mvm stats --format json     # machine-readable

v1 is point-in-time only (like "docker stats --no-stream"). Continuous
streaming is a follow-up; --no-stream is accepted for forward-compatibility
but is currently the only supported mode — omitting it behaves identically.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := resolveFormat(format, false)
			if err != nil {
				return err
			}
			return runStats(store, args, wantJSON)
		},
	}

	cmd.Flags().StringVar(&format, "format", "", "output format: table (default) or json")
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "disable streaming (default and only supported mode in v1)")

	return cmd
}

func runStats(store *state.Store, names []string, wantJSON bool) error {
	var all []server.VMStats

	// applevz: PID is the mvm-vz helper running natively on the macOS host
	// (see runStartAppleVZ's startResult.PID) — query it directly, no daemon
	// involved, matching every other applevz-vs-daemon split in this package
	// (list.go's localApplevzVMs, exec.go's runExecAppleVZ, etc.).
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}
	for _, vm := range localVMs {
		row := server.VMStats{Name: vm.Name, Backend: vm.Backend, PID: vm.PID, Status: vm.Status}
		if vm.Status == "running" && vm.PID > 0 {
			cpu, memMB, err := hostProcessStats(vm.PID)
			if err != nil {
				row.Error = err.Error()
			} else {
				row.CPUPct, row.MemMB = cpu, memMB
			}
		}
		all = append(all, row)
	}

	// Firecracker: best-effort daemon call, matching list.go's pattern — an
	// applevz-only host with no daemon running is not an error for `mvm
	// stats` either.
	if sc, err := requireDaemon(); err == nil {
		if stats, err := sc.StatsVMs(context.Background()); err == nil {
			all = append(all, stats...)
		}
	}

	if len(names) > 0 {
		want := make(map[string]bool, len(names))
		for _, n := range names {
			want[n] = true
		}
		var filtered []server.VMStats
		for _, row := range all {
			if want[row.Name] {
				filtered = append(filtered, row)
			}
		}
		all = filtered
	}

	if wantJSON {
		data, _ := json.MarshalIndent(all, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if len(all) == 0 {
		fmt.Println("No microVMs.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tBACKEND\tPID\tCPU%\tMEM(MiB)\tSTATUS")
	for _, row := range all {
		cpu, mem := "-", "-"
		if row.Error == "" && row.Status == "running" {
			cpu = fmt.Sprintf("%.1f", row.CPUPct)
			mem = fmt.Sprintf("%.0f", row.MemMB)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", row.Name, row.Backend, row.PID, cpu, mem, row.Status)
	}
	return w.Flush()
}

// hostProcessStats runs ps directly on the macOS host. Used for applevz
// VMs, whose PID is the mvm-vz helper process running natively on the host
// (see vm.StartResult.PID / runStartAppleVZ) — unlike the Firecracker
// backend's PID, which lives inside Lima's process namespace and must go
// through the daemon's Executor instead (see firecracker.ProcessStats,
// used by handleStatsVMs).
func hostProcessStats(pid int) (cpuPct, memMB float64, err error) {
	out, err := exec.Command("ps", "-o", "%cpu=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return firecracker.ParsePSOutput(string(out))
}
```

- [ ] **Step 10: Write the failing `runStats` filter test, then pass it**

Create `internal/cli/stats_test.go`. Since `runStats` shells out to the real daemon/host `ps`, keep this test scoped to the pure name-filtering behavior via a package-level seam — extract the filter into its own tested function rather than exercising `runStats` end-to-end (which needs a real store, daemon, and PIDs):

```go
package cli

import (
	"reflect"
	"testing"

	"github.com/agentstep/mvm/internal/server"
)

func TestFilterStatsByNameKeepsOnlyRequested(t *testing.T) {
	all := []server.VMStats{
		{Name: "web", Status: "running"},
		{Name: "worker", Status: "running"},
		{Name: "db", Status: "stopped"},
	}
	got := filterStatsByName(all, []string{"worker", "db"})
	want := []server.VMStats{
		{Name: "worker", Status: "running"},
		{Name: "db", Status: "stopped"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterStatsByName() = %+v, want %+v", got, want)
	}
}

func TestFilterStatsByNameEmptyMeansAll(t *testing.T) {
	all := []server.VMStats{{Name: "web"}, {Name: "worker"}}
	got := filterStatsByName(all, nil)
	if !reflect.DeepEqual(got, all) {
		t.Errorf("filterStatsByName(nil) = %+v, want unchanged %+v", got, all)
	}
}
```

Run: `go test ./internal/cli/ -run TestFilterStatsByName -v`
Expected: FAIL — `undefined: filterStatsByName`.

Refactor `runStats` in `internal/cli/stats.go` to extract the filter (replace the inline `if len(names) > 0 { ... }` block):

```go
// filterStatsByName keeps only rows whose Name is in names; an empty names
// means "keep everything" (mvm stats with no positional args = all VMs).
func filterStatsByName(all []server.VMStats, names []string) []server.VMStats {
	if len(names) == 0 {
		return all
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var filtered []server.VMStats
	for _, row := range all {
		if want[row.Name] {
			filtered = append(filtered, row)
		}
	}
	return filtered
}
```

and in `runStats`, replace the inline block with `all = filterStatsByName(all, names)`.

Run: `go test ./internal/cli/ -run TestFilterStatsByName -v`
Expected: PASS (2 tests).

- [ ] **Step 11: Register `mvm stats` in root.go**

In `internal/cli/root.go`'s `rootCmd.AddCommand(...)`, add `newStatsCmd(store),` directly after `newListCmd(store),`:

```go
		newListCmd(store),
		newStatsCmd(store),
		newInspectCmd(store),
```

- [ ] **Step 12: Full build + test**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -40`
Expected: clean build, `go vet` silent, no FAILs.

- [ ] **Step 13: Manual smoke-test**

Run: `go run ./cmd/mvm stats --help`
Expected: usage showing `mvm stats [name...]` with `--format` and `--no-stream`.

If a backend is available:
```bash
go run ./cmd/mvm start stattest
go run ./cmd/mvm stats
go run ./cmd/mvm stats stattest --format json
go run ./cmd/mvm delete stattest --force
```
Expected: a table (or JSON) row for `stattest` with a plausible CPU%/MEM reading while running. If no backend is available, skip and note it — not a blocker for the unit-tested portions.

- [ ] **Step 14: Commit**

```bash
git add internal/firecracker/stats.go internal/firecracker/stats_test.go \
  internal/server/routes.go internal/server/server.go internal/server/client.go \
  internal/server/schema_golden_test.go \
  internal/cli/stats.go internal/cli/stats_test.go internal/cli/root.go
git commit -m "feat: add mvm stats — point-in-time CPU/memory for running VMs"
```

---

### Task 7: Full-suite verification

**Files:** none (verification only).

**Interfaces:** none — this task runs the existing suite, it does not add code.

- [ ] **Step 1: Run the full module build, vet, and test suite**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -40`
Expected: clean build, `go vet` silent, every package `ok` (packages with hardware-dependent tests may skip; no FAILs).

- [ ] **Step 2: Confirm no existing command's behavior changed**

Run: `go test ./internal/cli/ -run 'TestParsePorts|TestRunRun|TestApplevzSpec|TestShellQuote|TestShellJoin' -v`
Run: `go test ./internal/server/ -run 'TestVMResponseSchemaGolden|TestVMInspectResponseSchemaGolden' -v`
Expected: PASS — these pre-existing tests, none of which this plan intentionally rewrote the assertions of (only `TestParsePorts` gained rows, and only in an additive way — every pre-existing row's expectation is untouched), must still pass.

- [ ] **Step 3: Confirm every new flag is discoverable**

Run: `go run ./cmd/mvm exec --help && go run ./cmd/mvm run --help && go run ./cmd/mvm start --help && go run ./cmd/mvm list --help && go run ./cmd/mvm inspect --help && go run ./cmd/mvm stats --help`
Expected: `--env-file` (exec, run), `--rm` (start, documented as unsupported), `-d`/`--detach` (exec), `--format` (list, inspect, stats), and the new `mvm stats` command itself all show up in `--help` output.

- [ ] **Step 4: Commit (only if Steps 1-3 required any fix)**

If everything already passed clean, there is nothing to commit here — skip. Otherwise:

```bash
git add -A
git commit -m "fix: address full-suite verification findings for container ergonomics alignment"
```

---

## Out of Scope (explicitly)

- **True detached exec with a retrievable exit code / output / kill-by-PID.** Task 5's `-d` is a shell-level background wrapper (`setsid ... &`), not a first-class agent primitive. A real implementation needs a new agent protocol request type (`agent/internal/protocol`) plus a "collect" verb to fetch a backgrounded job's later result — tracked as a follow-up, not attempted here.
- **Streaming `mvm stats`.** Task 6 ships `--no-stream`-only (point-in-time). A live/continuously-updating view needs a poll-and-diff loop (or a proper `/proc` sampling window) plus a terminal-redraw UI — a distinct, larger piece of work.
- **IPv6 host-ip literals in `-p` (`[::1]:8080:80`).** Task 4's `parsePorts` errors clearly on more than 3 colon-separated segments rather than attempting bracket-aware parsing.
- **Verifying, beyond static code reading, that the Firecracker/Lima `-d` iptables filter actually changes host-reachable bind behavior end-to-end.** Task 4 Step 13 flags this as a required-but-possibly-skipped manual check, not something this plan can assert from the code alone (see that task's Grounded finding).
- **`--rm` actually doing something on `mvm start`.** Task 3's decision is to reject it, permanently, not defer it — `mvm run` is the correct home for ephemeral semantics per the design spec's decision #5.
- **A generic `--format` mechanism for every other command with `--json`** (`build`, `pool status`, `snapshot list`, etc.). Task 2 scopes `--format` to `list` and `inspect` only, per the prompt; extending the pattern further is a natural but separate follow-up.
