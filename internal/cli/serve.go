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

func runServeStart(limaClient *lima.Client, store *state.Store, socketPath, listenAddr, tlsCert, tlsKey, apiKeyFlag, apiKeyFile string) error {
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
		ListenAddr: listenAddr,
		TLSCert:    tlsCert,
		TLSKey:     tlsKey,
		APIKey:     server.LoadAPIKey(apiKeyFlag, apiKeyFile),
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

// runServeStopE sends SIGTERM to the running mvm daemon. Shared by
// `system stop`.
func runServeStopE(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	pidPath := server.DefaultPIDPath()
	running, pid, _ := server.IsRunning(pidPath)
	if !running {
		fmt.Fprintln(out, "mvm daemon is not running")
		return nil
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to stop daemon (PID %d): %w", pid, err)
	}
	fmt.Fprintf(out, "mvm daemon stopped (PID %d)\n", pid)
	return nil
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
        <string>system</string>
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
