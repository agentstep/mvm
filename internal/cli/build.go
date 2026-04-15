package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/spf13/cobra"
)

func newBuildCmd() *cobra.Command {
	var (
		file   string
		tag    string
		sizeMB int
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build a custom rootfs image from a Dockerfile",
		Long: `Build a custom rootfs image by applying Dockerfile RUN and ENV steps
to the mvm base image. The result is cached and can be used with mvm start --image.

  mvm build -f Dockerfile -t my-image
  mvm build -f Dockerfile -t my-image --size 1024
  mvm start my-app --image my-image`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			if tag == "" {
				return fmt.Errorf("--tag (-t) is required")
			}
			return runBuild(file, tag, sizeMB)
		},
	}

	cmd.Flags().StringVarP(&file, "file", "f", "", "path to Dockerfile")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "image name/tag")
	cmd.Flags().IntVar(&sizeMB, "size", 512, "additional size in MiB to add to the image")

	return cmd
}

func runBuild(file, imageName string, sizeMB int) error {
	// Parse Dockerfile locally.
	steps, err := firecracker.ParseDockerfile(file)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("no supported build steps found in %s", file)
	}

	fmt.Printf("Parsed %d build step(s) from %s\n", len(steps), file)
	for i, s := range steps {
		preview := s.Args
		if len(preview) > 60 {
			preview = preview[:57] + "..."
		}
		fmt.Printf("  %d. %s %s\n", i+1, s.Directive, preview)
	}

	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	fmt.Printf("\nBuilding image '%s' (this may take several minutes)...\n", imageName)

	// Use a 10-minute timeout for the build operation.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := sc.Build(ctx, imageName, steps, sizeMB); err != nil {
		return err
	}

	fmt.Printf("  Image '%s' built successfully!\n", imageName)
	fmt.Printf("  Use it with: mvm start <name> --image %s\n", imageName)
	return nil
}
