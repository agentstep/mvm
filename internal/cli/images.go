package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newImagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "images",
		Short:   "Manage custom rootfs images",
		Aliases: []string{"image"},
	}

	cmd.AddCommand(
		newImageListCmd(),
		newImageDeleteCmd(),
	)

	return cmd
}

func newImageListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List custom rootfs images",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImageList(cmd.Context())
		},
	}
}

func newImageDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name>",
		Short:   "Delete a custom rootfs image",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImageDelete(cmd.Context(), args[0])
		},
	}
}

func runImageList(ctx context.Context) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	images, err := sc.ImageList(ctx)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		fmt.Println("No custom images. Build one with: mvm build -f Dockerfile -t <name>")
		return nil
	}

	fmt.Printf("%-20s %s\n", "NAME", "SIZE (MiB)")
	for _, img := range images {
		fmt.Printf("%-20s %d\n", img.Name, img.SizeMB)
	}
	return nil
}

func runImageDelete(ctx context.Context, name string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	if err := sc.ImageDelete(ctx, name); err != nil {
		return err
	}

	fmt.Printf("  Image '%s' deleted\n", name)
	return nil
}
