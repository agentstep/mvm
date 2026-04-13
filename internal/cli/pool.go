package cli

import (
	"fmt"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newPoolCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Manage the warm VM pool for instant starts",
	}

	cmd.AddCommand(
		newPoolWarmCmd(limaClient, store),
		newPoolStatusCmd(limaClient),
	)

	return cmd
}

func newPoolWarmCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "warm",
		Short: "Pre-boot VMs for instant start",
		RunE: func(cmd *cobra.Command, args []string) error {
			initialized, err := store.IsInitialized()
			if err != nil {
				return err
			}
			if !initialized {
				return fmt.Errorf("mvm is not initialized. Run: mvm init")
			}
			if err := limaClient.EnsureRunning(); err != nil {
				return err
			}

			fmt.Println("Warming VM pool...")
			if err := firecracker.WarmPool(limaClient); err != nil {
				return err
			}
			fmt.Println("  ✓ Pool ready")
			return nil
		},
	}
}

func newPoolStatusCmd(limaClient *lima.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show warm pool status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := limaClient.EnsureRunning(); err != nil {
				fmt.Println("Pool: unavailable (Lima not running)")
				return nil
			}

			ready, total := firecracker.PoolStatus(limaClient)
			fmt.Printf("Pool: %d/%d warm VMs ready\n", ready, total)
			return nil
		},
	}
}
