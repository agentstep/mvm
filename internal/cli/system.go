package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
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
		newSystemDFCmd(store),
		newSystemVersionCmd(version, commit, date),
		newSystemLogsCmd(),
		newSystemStartCmd(limaClient, store),
		newSystemStopCmd(),
		newSystemInstallCmd(),
		newSystemUninstallCmd(),
	)
	return cmd
}

type resourceItem struct {
	InUse bool
	Bytes uint64
}

func diskEntry(items []resourceItem) cfDiskEntry {
	var e cfDiskEntry
	for _, it := range items {
		e.Total++
		e.SizeInBytes += it.Bytes
		if it.InUse {
			e.Active++
		} else {
			e.Reclaimable += it.Bytes
		}
	}
	return e
}

// buildDiskUsage assembles the container-shaped cfDiskUsage. Pure so the
// active/reclaimable/total accounting is testable without a real data dir.
// Volumes stay zero until the volume noun lands (Slice 2).
func buildDiskUsage(containers, images []resourceItem) cfDiskUsage {
	return cfDiskUsage{Containers: diskEntry(containers), Images: diskEntry(images), Volumes: cfDiskEntry{}}
}

func newSystemDFCmd(store *state.Store) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "df",
		Short: "Show mvm disk usage (VMs, images, volumes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, images, err := collectDiskUsage(cmd.Context(), store)
			if err != nil {
				return err
			}
			du := buildDiskUsage(containers, images)
			if format == "json" {
				data, err := json.MarshalIndent(du, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "TYPE\tACTIVE\tTOTAL\tSIZE\tRECLAIMABLE")
			fmt.Fprintf(w, "Containers\t%d\t%d\t%d\t%d\n", du.Containers.Active, du.Containers.Total, du.Containers.SizeInBytes, du.Containers.Reclaimable)
			fmt.Fprintf(w, "Images\t%d\t%d\t%d\t%d\n", du.Images.Active, du.Images.Total, du.Images.SizeInBytes, du.Images.Reclaimable)
			fmt.Fprintf(w, "Volumes\t%d\t%d\t%d\t%d\n", du.Volumes.Active, du.Volumes.Total, du.Volumes.SizeInBytes, du.Volumes.Reclaimable)
			w.Flush()
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}

// collectDiskUsage gathers VM rootfs sizes (from the store) and image sizes
// (from the daemon, best-effort — an applevz-only host has no daemon). A
// container is in use when running; an image when a VM spec references it.
func collectDiskUsage(ctx context.Context, store *state.Store) (containers, images []resourceItem, err error) {
	vms, err := store.ListVMs()
	if err != nil {
		return nil, nil, err
	}
	inUseImage := make(map[string]bool)
	for _, vm := range vms {
		var b uint64
		if fi, statErr := os.Stat(vm.RootfsPath); statErr == nil {
			b = uint64(fi.Size())
		}
		containers = append(containers, resourceItem{InUse: vm.Status == "running", Bytes: b})
		if vm.Spec != nil && vm.Spec.Image != "" {
			inUseImage[vm.Spec.Image] = true
		}
	}
	if sc, derr := requireDaemon(); derr == nil {
		if imgs, lerr := sc.ImageList(ctx); lerr == nil {
			for _, img := range imgs {
				images = append(images, resourceItem{InUse: inUseImage[img.Name], Bytes: uint64(img.SizeMB) * 1024 * 1024})
			}
		}
	}
	return containers, images, nil
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
