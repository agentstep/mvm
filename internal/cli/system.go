package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newSystemCmd(limaClient *lima.Client, store *state.Store, version, commit, date string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "system",
		Short:   "Inspect and manage the mvm system and daemon",
		Aliases: []string{"s"},
	}
	// df (Task 19) is added to this AddCommand when that task lands.
	cmd.AddCommand(
		newSystemStatusCmd(limaClient, store, version),
		newSystemVersionCmd(version, commit, date),
		newSystemLogsCmd(),
		newSystemStartCmd(limaClient, store),
		newSystemStopCmd(),
		newSystemInstallCmd(),
		newSystemUninstallCmd(),
	)
	return cmd
}

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
// ("is running", "container-apiserver version: ", "application install root: ")
// so container-dashboard's system-status parser reads mvm unchanged. applevz has
// no daemon, so it says so honestly instead of faking one.
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

func newSystemInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the mvm daemon as a launchd login agent (auto-start on login)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return installServeLaunchd()
		},
	}
}

func newSystemUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the mvm daemon launchd login agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			return uninstallServeLaunchd()
		},
	}
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

// tailFile writes the contents of path to w. If follow is true it keeps
// polling for appended bytes until interrupted.
func tailFile(w io.Writer, path string, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("log file not found: %s", path)
		}
		return err
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	if !follow {
		return nil
	}

	for {
		time.Sleep(500 * time.Millisecond)
		n, err := io.Copy(w, f)
		if err != nil {
			return err
		}
		if n == 0 {
			// No new bytes; seek stays where io.Copy left off (at EOF), so the
			// next read picks up any freshly appended data.
			continue
		}
	}
}
