package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newExecCmd(store *state.Store) *cobra.Command {
	var (
		interactive bool
		tty         bool
		workdir     string
		envVars     []string
		user        string
	)

	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a command in a running microVM",
		Long: `Run a command inside a running microVM.

  mvm exec my-vm -- ls /
  mvm exec my-vm -it -- bash
  mvm exec my-vm -e FOO=bar -- env
  echo "data" | mvm exec my-vm -- cat`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			remoteArgs := args[1:]
			return runExec(store, name, remoteArgs, interactive || tty, workdir, envVars, user)
		},
	}

	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the VM")
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil, "set environment variables (KEY=VALUE)")
	cmd.Flags().StringVarP(&user, "user", "u", "", "run as user")

	return cmd
}

func runExec(store *state.Store, name string, remoteArgs []string, interactive bool, workdir string, envVars []string, user string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	script := buildExecScript(remoteArgs, workdir, envVars, user)

	if interactive {
		// Put the terminal in raw mode so keystrokes are forwarded
		// directly to the guest PTY without local echo or line buffering.
		fd := int(os.Stdin.Fd())
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("failed to set raw terminal: %w", err)
		}
		defer term.Restore(fd, oldState)

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
