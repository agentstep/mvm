package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newPoolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Manage the warm VM pool for instant starts",
	}

	cmd.AddCommand(
		newPoolWarmCmd(),
		newPoolStatusCmd(),
	)

	return cmd
}

func newPoolWarmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "warm",
		Short: "Pre-boot VMs for instant start",
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := requireDaemon()
			if err != nil {
				return err
			}

			fmt.Println("Warming VM pool...")
			ctx := context.Background()
			if err := sc.PoolWarm(ctx); err != nil {
				return err
			}
			fmt.Println("  ✓ Pool ready")
			return nil
		},
	}
}

func newPoolStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show warm pool status",
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := requireDaemon()
			if err != nil {
				return err
			}

			ctx := context.Background()
			status, err := sc.PoolStatus(ctx)
			if err != nil {
				fmt.Println("Pool: unavailable (daemon error)")
				return nil
			}
			fmt.Printf("Pool: %d/%d warm VMs ready\n", status.Ready, status.Total)
			return nil
		},
	}
}
