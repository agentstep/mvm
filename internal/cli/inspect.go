package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newInspectCmd(store *state.Store) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "inspect <name>",
		Short: "Display detailed information about a microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(store, args[0], format)
		},
	}

	cmd.Flags().StringVar(&format, "format", "json", "output format: json (default) or table")

	return cmd
}

func runInspect(store *state.Store, name string, format string) error {
	if format != "json" && format != "table" {
		return fmt.Errorf("invalid --format %q (want %q or %q)", format, "json", "table")
	}

	// applevz VMs live purely in local state — the daemon has never heard
	// of them (same backend split as `mvm list`).
	if vm, err := store.GetVM(name); err == nil && vm.Backend == "applevz" {
		return printInspectResult(server.InspectResponseFromVM(vm), format)
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	resp, err := sc.InspectVM(context.Background(), name)
	if err != nil {
		return err
	}
	return printInspectResult(*resp, format)
}

func printInspectResult(resp server.VMInspectResponse, format string) error {
	if format == "table" {
		return printInspectTable(resp)
	}
	return printInspect(resp)
}

func printInspect(resp server.VMInspectResponse) error {
	// container's inspect returns a JSON array the client reads [0] from.
	arr := []cfContainer{toCFContainer(resp.VMResponse, resp.Spec, true)}
	data, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// printInspectTable renders the same VMInspectResponse as a human-readable
// key: value summary — additive only, the JSON path (printInspect) is
// completely unchanged and stays the default.
func printInspectTable(resp server.VMInspectResponse) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Name:\t%s\n", resp.Name)
	fmt.Fprintf(w, "Status:\t%s\n", resp.Status)
	fmt.Fprintf(w, "Backend:\t%s\n", resp.Backend)
	fmt.Fprintf(w, "Guest IP:\t%s\n", resp.GuestIP)
	fmt.Fprintf(w, "PID:\t%d\n", resp.PID)
	fmt.Fprintf(w, "Created:\t%s\n", resp.CreatedAt.Format(time.RFC3339))
	if resp.Error != "" {
		fmt.Fprintf(w, "Error:\t%s\n", resp.Error)
	}
	for _, p := range resp.Ports {
		host := p.HostIP
		if host == "" {
			host = "localhost"
		}
		fmt.Fprintf(w, "Port:\t%s:%d -> %d/%s\n", host, p.HostPort, p.GuestPort, p.Proto)
	}
	if resp.Spec != nil {
		fmt.Fprintf(w, "Cpus:\t%d\n", resp.Spec.Cpus)
		fmt.Fprintf(w, "Memory:\t%d MiB\n", resp.Spec.MemoryMB)
		fmt.Fprintf(w, "Net Policy:\t%s\n", resp.Spec.NetPolicy)
		if resp.Spec.Image != "" {
			fmt.Fprintf(w, "Image:\t%s\n", resp.Spec.Image)
		}
	}
	return w.Flush()
}
