package cli

import (
	"fmt"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newDeleteCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	var (
		force bool
		all   bool
	)

	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a microVM and all its resources",
		Aliases: []string{"rm"},
		Args: func(cmd *cobra.Command, args []string) error {
			allFlag, _ := cmd.Flags().GetBool("all")
			if allFlag {
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("requires exactly 1 argument (or --all)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return runDeleteAll(limaClient, store, force)
			}
			return runDelete(limaClient, store, args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "stop the VM first if running")
	cmd.Flags().BoolVar(&all, "all", false, "delete all microVMs")

	return cmd
}

func runDelete(limaClient *lima.Client, store *state.Store, name string, force bool) error {
	vm, err := store.GetVM(name)
	if err != nil {
		return err
	}

	if vm.Status == "running" {
		if !force {
			return fmt.Errorf("microVM %q is running. Stop it first (mvm stop %s) or use --force", name, name)
		}
		fmt.Printf("Stopping microVM '%s'...\n", name)
	}

	// Ensure Lima is running for cleanup
	if err := limaClient.EnsureRunning(); err != nil {
		return err
	}

	fmt.Printf("Deleting microVM '%s'...\n", name)

	// Clean up all resources inside Lima
	if err := firecracker.Cleanup(limaClient, vm); err != nil {
		fmt.Printf("  Warning: cleanup error: %v\n", err)
	}
	fmt.Println("  ✓ Resources cleaned up")

	// Remove from state
	if err := store.RemoveVM(name); err != nil {
		return err
	}
	fmt.Println("  ✓ State removed")

	return nil
}

func runDeleteAll(limaClient *lima.Client, store *state.Store, force bool) error {
	vms, err := store.ListVMs()
	if err != nil {
		return err
	}
	if len(vms) == 0 {
		fmt.Println("No microVMs to delete.")
		return nil
	}

	// Check for running VMs
	if !force {
		for _, vm := range vms {
			if vm.Status == "running" {
				return fmt.Errorf("some microVMs are still running. Stop them first or use --force")
			}
		}
	}

	for _, vm := range vms {
		if err := runDelete(limaClient, store, vm.Name, force); err != nil {
			fmt.Printf("  Warning: failed to delete %s: %v\n", vm.Name, err)
		}
	}
	return nil
}
