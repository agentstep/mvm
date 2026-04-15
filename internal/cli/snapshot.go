package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snapshot",
		Short:   "Manage VM snapshots",
		Aliases: []string{"snap"},
	}

	cmd.AddCommand(
		newSnapshotCreateCmd(),
		newSnapshotRestoreCmd(),
		newSnapshotListCmd(),
		newSnapshotDeleteCmd(),
	)

	return cmd
}

func newSnapshotCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <vm-name> [snapshot-name]",
		Short: "Create a delta snapshot of a running VM",
		Long: `Create a snapshot capturing the current state of a running VM.

Set MVM_SNAPSHOT_KEY to encrypt snapshots with AES-256-GCM:
  export MVM_SNAPSHOT_KEY=mysecretkey
  mvm snapshot create my-app`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			vmName := args[0]
			snapName := vmName + "-snap"
			if len(args) > 1 {
				snapName = args[1]
			}
			return runSnapshotCreate(cmd.Context(), vmName, snapName)
		},
	}
}

func newSnapshotRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <vm-name> <snapshot-name>",
		Short: "Restore a VM from a snapshot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSnapshotRestore(cmd.Context(), args[0], args[1])
		},
	}
}

func newSnapshotListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List all snapshots",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSnapshotList(cmd.Context())
		},
	}
}

func newSnapshotDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <snapshot-name>",
		Short:   "Delete a snapshot",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSnapshotDelete(cmd.Context(), args[0])
		},
	}
}

func runSnapshotCreate(ctx context.Context, vmName, snapName string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	fmt.Printf("Creating snapshot '%s' of VM '%s'...\n", snapName, vmName)
	if err := sc.SnapshotCreate(ctx, vmName, snapName); err != nil {
		return err
	}

	fmt.Printf("  Snapshot '%s' created\n", snapName)
	return nil
}

func runSnapshotRestore(ctx context.Context, vmName, snapName string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	fmt.Printf("Restoring VM '%s' from snapshot '%s'...\n", vmName, snapName)
	if err := sc.SnapshotRestore(ctx, vmName, snapName); err != nil {
		return err
	}

	fmt.Printf("  VM '%s' restored from '%s'\n", vmName, snapName)
	return nil
}

func runSnapshotList(ctx context.Context) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	snapshots, err := sc.SnapshotList(ctx)
	if err != nil {
		return err
	}
	if len(snapshots) == 0 {
		fmt.Println("No snapshots.")
		return nil
	}

	for _, s := range snapshots {
		extra := ""
		if s.Type != "" {
			extra = fmt.Sprintf(" [%s]", s.Type)
		}
		fmt.Printf("  %s%s\n", s.Name, extra)
	}
	return nil
}

func runSnapshotDelete(ctx context.Context, snapName string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	if err := sc.SnapshotDelete(ctx, snapName); err != nil {
		return err
	}

	fmt.Printf("  Snapshot '%s' deleted\n", snapName)
	return nil
}
