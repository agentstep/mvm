package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

const serveLaunchdLabel = "com.mvm.serve"

func newServeCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Manage the mvm daemon (fast exec via persistent connections)",
	}

	cmd.AddCommand(
		newServeStartCmd(limaClient, store),
		newServeStopCmd(),
		newServeStatusCmd(),
		newServeInstallCmd(),
		newServeUninstallCmd(),
	)

	return cmd
}

func newServeStartCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	var socketPath string

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the mvm daemon",
		Long: `Start the mvm daemon. Holds persistent vsock connections to running VMs
and exposes an HTTP API on a Unix socket for fast exec.

  mvm serve start                          # start in foreground
  curl --unix-socket ~/.mvm/server.sock http://mvm/health`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeStart(limaClient, store, socketPath)
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (default: ~/.mvm/server.sock)")
	return cmd
}

func runServeStart(limaClient *lima.Client, store *state.Store, socketPath string) error {
	// Detect environment: inside Lima = LocalExecutor, macOS = lima.Client
	var executor firecracker.Executor
	if server.IsLinux() {
		executor = &firecracker.LocalExecutor{}
	} else {
		executor = limaClient
	}
	// State path is the same everywhere (shared via writable virtiofs mount)

	cfg := server.Config{
		SocketPath: socketPath,
		Store:      store,
		Executor:   executor,
	}

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	return srv.Start(ctx)
}

func newServeStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the mvm daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := server.DefaultPIDPath()
			running, pid, _ := server.IsRunning(pidPath)
			if !running {
				fmt.Println("mvm daemon is not running")
				return nil
			}

			process, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			if err := process.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("failed to stop daemon (PID %d): %w", pid, err)
			}
			fmt.Printf("mvm daemon stopped (PID %d)\n", pid)
			return nil
		},
	}
}

func newServeStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show mvm daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := server.DefaultClient()
			if c.IsAvailable() {
				pidPath := server.DefaultPIDPath()
				_, pid, _ := server.IsRunning(pidPath)
				fmt.Printf("mvm daemon: running (PID %d)\n", pid)
				fmt.Printf("  Socket: %s\n", server.DefaultSocketPath())
			} else {
				fmt.Println("mvm daemon: not running")
				fmt.Println("  Start with: mvm serve start")
			}
			return nil
		},
	}
}

func newServeInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install mvm daemon as a launchd agent (auto-start on login)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return installServeLaunchd()
		},
	}
}

func newServeUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove mvm daemon launchd agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			return uninstallServeLaunchd()
		},
	}
}

func servePlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", serveLaunchdLabel+".plist")
}

func installServeLaunchd() error {
	mvmBin, err := os.Executable()
	if err != nil {
		mvmBin = "mvm"
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>serve</string>
        <string>start</string>
    </array>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/serve.log</string>
    <key>StandardErrorPath</key>
    <string>%s/serve.log</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
</dict>
</plist>`, serveLaunchdLabel, mvmBin, mvmDir, mvmDir, mvmDir)

	path := servePlistPath()
	os.MkdirAll(filepath.Dir(path), 0o755)

	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}

	exec.Command("launchctl", "unload", path).Run()
	if err := exec.Command("launchctl", "load", path).Run(); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}

	fmt.Println("  mvm daemon installed as launchd agent")
	fmt.Printf("    Plist: %s\n", path)
	fmt.Printf("    Log:   %s/serve.log\n", mvmDir)
	return nil
}

func uninstallServeLaunchd() error {
	path := servePlistPath()
	exec.Command("launchctl", "unload", path).Run()
	os.Remove(path)
	fmt.Println("  mvm daemon launchd agent removed")
	return nil
}
