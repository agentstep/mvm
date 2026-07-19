package cli

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

// StartupSpec is a declarative recipe run after a VM boots and its agent is
// reachable: clone a repo, set env, run commands (some in the background),
// then wait until a port answers. Each phase is timed and folded into the
// BootResult, so you can see where the time went.
//
// Loaded from a JSON file via `mvm start --startup recipe.json`.
type StartupSpec struct {
	Git      *GitSpec          `json:"git,omitempty"`
	Workdir  string            `json:"workdir,omitempty"` // default /workspace
	Env      map[string]string `json:"env,omitempty"`
	Secrets  []string          `json:"secrets,omitempty"` // attached secret names (merged with --secret)
	Commands []StartupCommand  `json:"commands,omitempty"`
	Ready    *ReadySpec        `json:"ready,omitempty"`
}

type GitSpec struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

type StartupCommand struct {
	Name       string `json:"name"`
	Run        string `json:"run"`
	Background bool   `json:"background,omitempty"` // detach (e.g. a dev server)
}

type ReadySpec struct {
	HTTP           string `json:"http"`                      // in-guest URL to poll
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default 30
}

func loadStartupSpec(path string) (*StartupSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read startup spec: %w", err)
	}
	var spec StartupSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse startup spec %s: %w", path, err)
	}
	if spec.Workdir == "" {
		spec.Workdir = "/workspace"
	}
	return &spec, nil
}

// envPrefix builds an `export K='v'; ...` prefix applied to every command so
// env is available to foreground and background steps alike.
func (s *StartupSpec) envPrefix() string {
	if len(s.Env) == 0 {
		return ""
	}
	var b strings.Builder
	for k, v := range s.Env {
		fmt.Fprintf(&b, "export %s=%s; ", k, shellQuote(v))
	}
	return b.String()
}

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

// runStartupRecipe executes the recipe over the guest agent, timing each phase
// into the supplied phaseTimer. logf receives human progress. Returns the first
// error (a failing foreground command aborts the recipe).
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
		if output, exitCode, err := agent.Exec(ctx, clone, ""); err != nil {
			return fmt.Errorf("git clone: %w", err)
		} else if exitCode != 0 {
			return fmt.Errorf("git clone failed (exit %d): %s", exitCode, strings.TrimSpace(output))
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
			// Detach so a long-running server doesn't block the recipe.
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
		// Poll in-guest so we don't need host→guest networking. Prefer wget
		// (busybox), fall back to curl.
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
