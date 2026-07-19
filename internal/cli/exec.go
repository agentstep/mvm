package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agentstep/mvm/internal/state"
	vm_pkg "github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

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

// runExecAppleVZ runs a command on an Apple VZ VM via the per-VM mvm-vz
// helper's vsock-bridged agent (no daemon). MVP: non-interactive only.
func runExecAppleVZ(store *state.Store, vm *state.VM, remoteArgs []string, interactive bool, workdir string, envVars []string, user string) error {
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

	// Forward piped/redirected stdin (echo data | mvm exec vm -- cat, or
	// mvm exec vm -- cat < file). Only attempt this when stdin is not an
	// interactive tty, and bound the read: an inherited fd that never reaches
	// EOF (a held-open pipe under CI/cron, or /dev/null variants) must not be
	// able to hang exec. A real pipe/file delivers its data + EOF promptly, so
	// the select returns as soon as the data is in — the timeout only fires for
	// a source that is never going to send anything.
	stdin := readStdinNonBlocking()

	// Inject the VM's attached secrets, decrypted from host memory at call time.
	// They go in as env exports and are never written to a guest file.
	if len(vm.Secrets) > 0 {
		secretEnv, err := secretEnvVars(vm.Secrets)
		if err != nil {
			return fmt.Errorf("load secrets for %q: %w", vm.Name, err)
		}
		envVars = append(envVars, secretEnv...)
	}

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

// runExecAppleVZInteractive runs an interactive PTY session against an Apple VZ
// VM: it puts the local terminal in raw mode, forwards SIGWINCH resizes, and
// relays the terminal to the guest agent's PTY over vsock until the command exits.
func runExecAppleVZInteractive(store *state.Store, vm *state.VM, remoteArgs []string, workdir string, envVars []string, user string) error {
	AutoResumeIfPaused(nil, store, vm)
	TouchActivity(store, vm.Name)

	script := buildExecScript(remoteArgs, workdir, envVars, user)

	fd := int(os.Stdin.Fd())
	var rows, cols uint16 = 24, 80
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("set raw terminal: %w", err)
		}
		defer term.Restore(fd, oldState)
		if w, h, err := term.GetSize(fd); err == nil {
			cols, rows = uint16(w), uint16(h)
		}
	}

	// Forward terminal resizes to the guest PTY.
	resize := make(chan [2]uint16, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if w, h, err := term.GetSize(fd); err == nil {
				select {
				case resize <- [2]uint16{uint16(h), uint16(w)}:
				default:
				}
			}
		}
	}()

	code, err := vm_pkg.NewAppleVZBackend(mvmDir).AgentClient(vm.Name).ExecInteractive(
		context.Background(), script, rows, cols, os.Getenv("TERM"), nil, os.Stdin, os.Stdout, resize)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

// readStdinNonBlocking reads piped/redirected stdin for a non-interactive
// exec, bounded so a never-closing fd can't hang the command. Returns "" for
// an interactive terminal, or if no data/EOF arrives before the deadline.
// A real pipe or file delivers its data and EOF promptly; the timeout only
// fires for a source that will never send anything (e.g. an inherited,
// held-open fd under CI/cron).
func readStdinNonBlocking() string {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return ""
	}
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(os.Stdin)
		done <- string(b)
	}()
	select {
	case s := <-done:
		return s
	case <-time.After(500 * time.Millisecond):
		return ""
	}
}

func buildExecScript(remoteArgs []string, workdir string, envVars []string, user string) string {
	var script strings.Builder
	if len(envVars) > 0 {
		for _, e := range envVars {
			script.WriteString(fmt.Sprintf("export %s; ", shellQuote(e)))
		}
	}
	if workdir != "" {
		script.WriteString(fmt.Sprintf("cd %s; ", shellQuote(workdir)))
	}
	if user != "" {
		innerCmd := shellQuote(shellJoin(remoteArgs))
		script.WriteString(fmt.Sprintf("su - %s -c %s", shellQuote(user), innerCmd))
	} else {
		for i, arg := range remoteArgs {
			if i > 0 {
				script.WriteByte(' ')
			}
			script.WriteString(shellQuote(arg))
		}
	}
	return script.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}
