package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newListCmd(store *state.Store) *cobra.Command {
	var (
		jsonOutput bool
		quiet      bool
		all        bool
		format     string
	)

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List microVMs (running only by default; -a for all)",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := resolveFormat(format, jsonOutput)
			if err != nil {
				return err
			}
			return runList(store, wantJSON, quiet, all)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON (alias for --format json)")
	cmd.Flags().StringVar(&format, "format", "", "output format: table (default) or json")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "only print VM names")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped VMs (default: running only)")

	return cmd
}

func runList(store *state.Store, jsonOutput, quiet, all bool) error {
	// Apple VZ VMs live purely in local state — the Firecracker daemon has
	// never heard of them (same reason exec/start/stop/pause special-case
	// applevz instead of going through requireDaemon()). Read them directly
	// rather than requiring the daemon, which previously made `mvm ls`
	// report "No microVMs" for a host with real, running applevz VMs.
	localVMs, err := localApplevzVMs(store)
	if err != nil {
		return err
	}

	// Daemon VMs (Firecracker) are best-effort: a host running applevz-only
	// has no daemon, and that's not an error for `mvm ls`.
	var daemonVMs []server.VMResponse
	if sc, err := requireDaemon(); err == nil {
		if vms, err := sc.ListVMs(context.Background()); err == nil {
			daemonVMs = vms
		}
	}

	// Container semantics: `list` shows running VMs only unless -a/--all is set.
	vms := filterRunning(mergeVMResponses(localVMs, daemonVMs), all)

	// Sort by creation time
	sort.Slice(vms, func(i, j int) bool {
		return vms[i].CreatedAt.Before(vms[j].CreatedAt)
	})

	if jsonOutput {
		data, _ := json.MarshalIndent(toCFContainers(vms, specsByName(store)), "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if len(vms) == 0 {
		if !quiet {
			fmt.Println("No microVMs. Create one with: mvm create <name>")
		}
		return nil
	}

	if quiet {
		for _, vm := range vms {
			fmt.Println(vm.Name)
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tIP\tBACKEND\tCREATED")
	for _, vm := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			vm.Name, vm.Status, vm.GuestIP, vm.Backend, timeAgo(vm.CreatedAt))
	}
	w.Flush()
	return nil
}

// localApplevzVMs reads applevz VMs straight from the local state store and
// shapes them into the same VMResponse the daemon returns for Firecracker
// VMs, so both backends render through one code path.
func localApplevzVMs(store *state.Store) ([]server.VMResponse, error) {
	all, err := store.ListVMs()
	if err != nil {
		return nil, err
	}
	var out []server.VMResponse
	for _, vm := range all {
		if vm.Backend != "applevz" {
			continue
		}
		out = append(out, server.VMResponse{
			Name:      vm.Name,
			Status:    vm.Status,
			GuestIP:   vm.GuestIP,
			PID:       vm.PID,
			Backend:   vm.Backend,
			Ports:     vm.Ports,
			CreatedAt: vm.CreatedAt,
		})
	}
	return out, nil
}

// mergeVMResponses combines local (applevz) and daemon (Firecracker) VM
// lists into one. Split out from runList so the merge/dedup logic is
// testable without a real daemon or state store on disk.
func mergeVMResponses(local, daemon []server.VMResponse) []server.VMResponse {
	if len(local) == 0 {
		return daemon
	}
	if len(daemon) == 0 {
		return local
	}
	seen := make(map[string]bool, len(local))
	out := make([]server.VMResponse, 0, len(local)+len(daemon))
	for _, vm := range local {
		seen[vm.Name] = true
		out = append(out, vm)
	}
	for _, vm := range daemon {
		if seen[vm.Name] {
			continue
		}
		out = append(out, vm)
	}
	return out
}

// filterRunning drops stopped/paused VMs unless all is set (container default).
func filterRunning(vms []server.VMResponse, all bool) []server.VMResponse {
	if all {
		return vms
	}
	out := make([]server.VMResponse, 0, len(vms))
	for _, vm := range vms {
		if vm.Status == "running" {
			out = append(out, vm)
		}
	}
	return out
}

// specsByName maps VM name → persisted spec, so container-shaped JSON can
// populate image/cpus/memory that VMResponse omits.
func specsByName(store *state.Store) map[string]*state.VMSpec {
	m := map[string]*state.VMSpec{}
	all, err := store.ListVMs()
	if err != nil {
		return m
	}
	for _, vm := range all {
		if vm.Spec != nil {
			m[vm.Name] = vm.Spec
			continue
		}
		m[vm.Name] = &state.VMSpec{Cpus: vm.Cpus, MemoryMB: vm.MemoryMB}
	}
	return m
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
