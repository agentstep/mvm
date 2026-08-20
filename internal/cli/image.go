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
	cmd.AddCommand(newImageLsCmd(), newImageRmCmd(), newImageInspectCmd(), newImagePruneCmd(store))
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

func newImageInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show detailed information for one custom rootfs image (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runImageInspect(cmd.Context(), args[0]) },
	}
}

func imageToCF(img server.ImageInfo) cfImage {
	return cfImage{
		Reference: img.Name,
		Descriptor: cfDescriptor{
			Digest: img.Digest,
			Size:   int64(img.SizeMB) * 1024 * 1024,
		},
	}
}

func findImage(imgs []server.ImageInfo, name string) (server.ImageInfo, error) {
	for _, img := range imgs {
		if img.Name == name {
			return img, nil
		}
	}
	return server.ImageInfo{}, fmt.Errorf("image %q not found", name)
}

// imagesToCF transforms ImageInfo (MiB) into cfImage (bytes). Pure.
//
// Digest is empty on this list path and populated on the inspect path: the
// digest is a sha256 of the whole ext4 blob, computed on demand by the daemon,
// and hashing every image just to list them would make `image ls` scale with
// total image bytes. docs/container-compat-matrix.md blesses the empty value
// here ("emit empty (dashboard tolerates) or synthesize a sha256"), so the key
// is emitted-but-empty rather than omitted — the container CLI always includes
// it, and dropping it would diverge from the shape this type exists to match.
func imagesToCF(imgs []server.ImageInfo) []cfImage {
	out := make([]cfImage, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, imageToCF(img))
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

func runImageInspect(ctx context.Context, name string) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	info, err := sc.ImageInspect(ctx, name)
	if err != nil {
		if !server.IsRouteMissing(err) {
			return err
		}
		// The daemon predates GET /v1/images/{name}. The CLI (macOS) and the
		// daemon (inside Lima) are upgraded separately, so this is routine
		// skew, not a broken install — fall back to the list route every
		// daemon version has. The digest is empty on that path, which is the
		// documented pre-digest shape rather than a hard failure.
		imgs, listErr := sc.ImageList(ctx)
		if listErr != nil {
			// Neither route exists, so the daemon predates images entirely.
			// Say that rather than surfacing a bare "404 Not Found", which
			// gives the user nothing to act on.
			if server.IsRouteMissing(listErr) {
				return fmt.Errorf("this daemon is too old to support images " +
					"(neither the inspect nor the list endpoint exists). " +
					"Upgrade it with: mvm system install")
			}
			return err
		}
		found, findErr := findImage(imgs, name)
		if findErr != nil {
			return findErr
		}
		info = &found
	}
	data, err := json.MarshalIndent([]cfImage{imageToCF(*info)}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func newImagePruneCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Remove custom rootfs images not referenced by any VM",
		RunE:  func(cmd *cobra.Command, args []string) error { return runImagePrune(cmd.Context(), store) },
	}
}

func unreferencedImages(imgs []server.ImageInfo, vms []*state.VM) []string {
	inUse := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if vm.Spec != nil && vm.Spec.Image != "" {
			inUse[vm.Spec.Image] = true
		}
	}
	var out []string
	for _, img := range imgs {
		if !inUse[img.Name] {
			out = append(out, img.Name)
		}
	}
	return out
}

func runImagePrune(ctx context.Context, store *state.Store) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}
	images, err := sc.ImageList(ctx)
	if err != nil {
		return err
	}
	vms, err := store.ListVMs()
	if err != nil {
		return err
	}
	unused := unreferencedImages(images, vms)
	if len(unused) == 0 {
		fmt.Println("No unused images to prune.")
		return nil
	}
	for _, name := range unused {
		if err := sc.ImageDelete(ctx, name); err != nil {
			return fmt.Errorf("remove %q: %w", name, err)
		}
		fmt.Printf("  Removed image '%s'\n", name)
	}
	return nil
}
