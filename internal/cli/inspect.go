package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newInspectCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Display detailed information about a microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(store, args[0])
		},
	}
}

func runInspect(store *state.Store, name string) error {
	// applevz VMs live purely in local state — the daemon has never heard
	// of them (same backend split as `mvm list`).
	if vm, err := store.GetVM(name); err == nil && vm.Backend == "applevz" {
		return printInspect(server.InspectResponseFromVM(vm))
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	resp, err := sc.InspectVM(context.Background(), name)
	if err != nil {
		return err
	}
	return printInspect(*resp)
}

func printInspect(resp server.VMInspectResponse) error {
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
