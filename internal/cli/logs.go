package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newLogsCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
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
			return runLogs(limaClient, store, args[0], follow, boot, tail)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	cmd.Flags().BoolVar(&boot, "boot", false, "show VM boot/console log")
	cmd.Flags().IntVarP(&tail, "tail", "n", 0, "number of lines from end")

	return cmd
}

func runLogs(limaClient *lima.Client, store *state.Store, name string, follow, boot bool, tail int) error {
	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}

	if boot {
		if vm.Backend == "applevz" {
			return showLocalLog(filepath.Join(mvmDir, "vms", vm.Name, "console.log"), follow, tail)
		}
		return showBootLog(limaClient, vm, follow, tail)
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
		// Follow mode needs interactive TTY
		return runExec(store, name, []string{"sh", "-c", logCmd}, true, "", nil, "")
	}

	if tail > 0 {
		logCmd = fmt.Sprintf("tail -n %d /var/log/messages 2>/dev/null || dmesg | tail -n %d", tail, tail)
	} else {
		logCmd = "cat /var/log/messages 2>/dev/null || dmesg"
	}
	return runExec(store, name, []string{"sh", "-c", logCmd}, false, "", nil, "")
}

func showBootLog(limaClient *lima.Client, vm *state.VM, follow bool, tail int) error {
	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	logPath := filepath.Join(firecracker.VMDir(vm.Name), "firecracker.log")

	var cmd string
	if follow {
		if tail > 0 {
			cmd = fmt.Sprintf("sudo tail -n %d -f %s", tail, logPath)
		} else {
			cmd = fmt.Sprintf("sudo tail -f %s", logPath)
		}
		return limaClient.ShellInteractive(cmd)
	}

	if tail > 0 {
		cmd = fmt.Sprintf("sudo tail -n %d %s", tail, logPath)
	} else {
		cmd = fmt.Sprintf("sudo cat %s", logPath)
	}
	out, err := limaClient.ShellWithTimeout(cmd, lima.LongTimeout)
	if err != nil {
		return fmt.Errorf("read boot log: %w", err)
	}
	fmt.Print(out)
	return nil
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
