package cli

import (
	"encoding/json"
	"fmt"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newNetworkCmd(store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Inspect mvm networks (read-only; mvm has one implicit default network)",
	}
	cmd.AddCommand(newNetworkLsCmd(store), newNetworkInspectCmd(store))
	return cmd
}

func newNetworkLsCmd(store *state.Store) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List networks",
		Aliases: []string{"list"},
		RunE:    func(cmd *cobra.Command, args []string) error { return runNetworkLs(store, format) },
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}

func newNetworkInspectCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show network details (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runNetworkInspect(store, args[0]) },
	}
}

func runNetworkLs(store *state.Store, format string) error {
	// mvm has exactly one network: the implicit "default".
	nets := []cfNetwork{defaultNetwork(store.GetBackend())}
	if format == "json" {
		data, err := json.MarshalIndent(nets, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("%-10s %-8s %-6s %s\n", "ID", "STATE", "MODE", "SUBNET")
	for _, n := range nets {
		fmt.Printf("%-10s %-8s %-6s %s\n", n.ID, n.State, n.Config.Mode, n.Status.IPv4Subnet)
	}
	return nil
}

func runNetworkInspect(store *state.Store, name string) error {
	if name != "default" {
		return fmt.Errorf("no network named %q (mvm has one network: default)", name)
	}
	data, err := json.MarshalIndent(defaultNetwork(store.GetBackend()), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
