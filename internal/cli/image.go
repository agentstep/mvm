package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newImageCmd(store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image",
		Short:   "Manage custom rootfs images",
		Aliases: []string{"i"},
	}
	cmd.AddCommand(newImageLsCmd(), newImageRmCmd())
	return cmd
}

func newImageLsCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List custom rootfs images",
		Aliases: []string{"list"},
		RunE:    func(cmd *cobra.Command, args []string) error { return runImageLs(cmd.Context(), format) },
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}

func newImageRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a custom rootfs image",
		Aliases: []string{"delete"},
		Args:    cobra.ExactArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return runImageRm(cmd.Context(), args[0]) },
	}
}

// imagesToCF transforms ImageInfo (MiB) into cfImage (bytes). Pure; digest is
// empty until the OCI image store lands (Slice 3).
func imagesToCF(imgs []server.ImageInfo) []cfImage {
	out := make([]cfImage, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, cfImage{Reference: img.Name, Descriptor: cfDescriptor{Size: int64(img.SizeMB) * 1024 * 1024}})
	}
	return out
}

func runImageLs(ctx context.Context, format string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	images, err := sc.ImageList(ctx)
	if err != nil {
		return err
	}
	if format == "json" {
		data, err := json.MarshalIndent(imagesToCF(images), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	if len(images) == 0 {
		fmt.Println("No custom images. Build one with: mvm build -f Dockerfile -t <name>")
		return nil
	}
	fmt.Printf("%-20s %s\n", "REFERENCE", "SIZE (MiB)")
	for _, img := range images {
		fmt.Printf("%-20s %d\n", img.Name, img.SizeMB)
	}
	return nil
}

func runImageRm(ctx context.Context, name string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	if err := sc.ImageDelete(ctx, name); err != nil {
		return err
	}
	fmt.Printf("  Image '%s' removed\n", name)
	return nil
}
