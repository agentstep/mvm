package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newExecCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
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
			return runExec(limaClient, store, name, remoteArgs, interactive || tty, workdir, envVars, user)
		},
	}

	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the VM")
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil, "set environment variables (KEY=VALUE)")
	cmd.Flags().StringVarP(&user, "user", "u", "", "run as user")

	return cmd
}

func runExec(limaClient *lima.Client, store *state.Store, name string, remoteArgs []string, interactive bool, workdir string, envVars []string, user string) error {
	script := buildExecScript(remoteArgs, workdir, envVars, user)

	// Fast path: try mvm daemon (non-interactive only)
	if !interactive {
		sc := server.DefaultClient()
		if sc.IsAvailable() {
			exitCode, err := sc.ExecStream(context.Background(), name, script, os.Stdout, os.Stderr)
			if err == nil {
				if exitCode != 0 {
					return fmt.Errorf("exit code %d", exitCode)
				}
				return nil
			}
		}
	}

	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}
	if vm.Status == "paused" {
		AutoResumeIfPaused(limaClient, store, vm)
		vm.Status = "running"
	}
	if vm.Status != "running" {
		return fmt.Errorf("microVM %q is not running (status: %s)", name, vm.Status)
	}

	TouchActivity(store, name)

	// Apple VZ path
	if vm.Backend == "applevz" {
		return runExecDirect(vm, remoteArgs, interactive, workdir, envVars, user)
	}

	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	if !firecracker.IsRunning(limaClient, vm.PID) {
		return fmt.Errorf("microVM %q process is not running (PID %d)", name, vm.PID)
	}

	if !interactive {
		// Fallback: exec via limaClient.Shell → exec-direct (direct TCP inside Lima)
		b64Cmd := base64.StdEncoding.EncodeToString([]byte(script))
		cmd := fmt.Sprintf("/opt/mvm/mvm-daemon exec-direct %s %s", vm.GuestIP, b64Cmd)
		out, err := limaClient.ShellWithTimeout(cmd, lima.LongTimeout)
		if err != nil {
			return fmt.Errorf("exec failed: %w", err)
		}
		fmt.Print(out)
		return nil
	}

	// Interactive (TTY) — SSH for PTY support
	keyPath := filepath.Join(firecracker.KeyDir, "mvm.id_ed25519")
	sshCmd := fmt.Sprintf("sudo ssh -i %s -t -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR root@%s %s",
		keyPath, vm.GuestIP, shellQuote(script))
	return limaClient.ShellInteractive(sshCmd)
}

// runExecDirect runs a command via SSH directly from macOS (Apple VZ backend).
func runExecDirect(vm *state.VM, remoteArgs []string, interactive bool, workdir string, envVars []string, user string) error {
	keyPath := filepath.Join(mvmDir, "keys", "mvm.id_ed25519")
	script := buildExecScript(remoteArgs, workdir, envVars, user)

	sshFlags := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	if interactive {
		sshFlags = append(sshFlags, "-t")
	}
	sshFlags = append(sshFlags, fmt.Sprintf("root@%s", vm.GuestIP), script)

	cmd := exec.Command("ssh", sshFlags...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
