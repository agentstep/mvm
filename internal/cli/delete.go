package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newDeleteCmd() *cobra.Command {
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
				return runDeleteAll(force)
			}
			return runDelete(args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "stop the VM first if running")
	cmd.Flags().BoolVar(&all, "all", false, "delete all microVMs")

	return cmd
}

func runDelete(name string, force bool) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Check VM status via listing
	vms, err := sc.ListVMs(ctx)
	if err != nil {
		return err
	}

	var found bool
	var status string
	for _, vm := range vms {
		if vm.Name == name {
			found = true
			status = vm.Status
			break
		}
	}
	if !found {
		return fmt.Errorf("microVM %q not found", name)
	}

	if status == "running" {
		if !force {
			return fmt.Errorf("microVM %q is running. Stop it first (mvm stop %s) or use --force", name, name)
		}
		fmt.Printf("Stopping microVM '%s'...\n", name)
		if err := sc.StopVM(ctx, name, true); err != nil {
			return err
		}
	}

	fmt.Printf("Deleting microVM '%s'...\n", name)
	if err := sc.DeleteVM(ctx, name); err != nil {
		return err
	}
	fmt.Println("  ✓ Deleted")

	return nil
}

func runDeleteAll(force bool) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()
	vms, err := sc.ListVMs(ctx)
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
		if err := runDelete(vm.Name, force); err != nil {
			fmt.Printf("  Warning: failed to delete %s: %v\n", vm.Name, err)
		}
	}
	return nil
}
