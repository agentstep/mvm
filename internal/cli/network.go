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

// networkUp reports whether mvm's implicit network actually exists on this
// host. It is not a constant: before `mvm init` there are no TAP devices and
// no NAT rules, and on the Firecracker backend the whole network lives inside
// Lima, so a stopped Lima VM (or a stopped daemon) means an unreachable subnet.
func networkUp(store *state.Store) bool {
	initialized, err := store.IsInitialized()
	if err != nil || !initialized {
		return false
	}
	if store.GetBackend() == "firecracker" {
		// The subnet is inside Lima; reaching the daemon is the cheapest
		// proof that Lima is up and the network was provisioned.
		_, err := requireDaemon()
		return err == nil
	}
	return true
}

func runNetworkLs(store *state.Store, format string) error {
	wantJSON, err := resolveFormat(format, false)
	if err != nil {
		return err
	}
	// mvm has exactly one network: the implicit "default".
	nets := []cfNetwork{defaultNetwork(store.GetBackend(), networkUp(store))}
	if wantJSON {
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
	// A 1-element array, not a bare object: container's inspect returns an
	// array the client reads [0] from (see inspect.go's printInspect). A
	// dashboard adapter reusing its existing parser indexes [0], so emitting
	// an object here reads as a type error or an empty result.
	arr := []cfNetwork{defaultNetwork(store.GetBackend(), networkUp(store))}
	data, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
