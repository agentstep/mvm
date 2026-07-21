package cli

import (
	"fmt"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newCreateCmd(store *state.Store) *cobra.Command {
	var (
		image     string
		cpus      int
		memoryMB  int
		netPolicy string
		ports     []string
		volumes   []string
		seccomp   string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Provision a microVM and leave it stopped",
		Long: `Provision a microVM (allocate config, prepare rootfs) and leave it stopped.

The VM is booted once to lay down its rootfs and network allocation, then
stopped — start it later with: mvm start <name>.

  mvm create mybox
  mvm create mybox --image my-image -c 4 -m 2048
  mvm create web -p 8080:80 -v ./src:/app`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			portMaps, err := parsePorts(ports)
			if err != nil {
				return err
			}
			vols, err := parseVolumes(volumes)
			if err != nil {
				return err
			}
			return runCreate(store, args[0], image, cpus, memoryMB, netPolicy, portMaps, vols, seccomp)
		},
	}

	cmd.Flags().StringVar(&image, "image", "", "custom rootfs image name; \"base\" or empty = default rootfs")
	cmd.Flags().IntVarP(&cpus, "cpus", "c", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVarP(&memoryMB, "memory", "m", 0, "RAM in MiB (default: 1024)")
	cmd.Flags().StringVar(&netPolicy, "net-policy", "open", "network policy: open, deny, or allow:domain1,domain2")
	cmd.Flags().StringArrayVarP(&ports, "publish", "p", nil, "publish port (hostPort:guestPort[/proto])")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "v", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().StringVar(&seccomp, "seccomp", "", "seccomp profile: strict, moderate, or permissive")

	return cmd
}

// runCreate provisions a VM and leaves it stopped. There is no
// create-without-boot path on either backend, so create honestly boots then
// stops — the rootfs clone and net allocation persist, and the VM is parked
// "stopped" ready for `mvm start`.
func runCreate(store *state.Store, name, image string, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, seccomp string) error {
	existing, err := existingVMNames(store)
	if err != nil {
		return err
	}
	if existing[name] {
		return fmt.Errorf("microVM %q already exists", name)
	}
	if err := runStart(store, name, true, ports, netPolicy, volumes, seccomp, "", cpus, memoryMB, resolveImage(image), false, nil, nil, true); err != nil {
		return fmt.Errorf("create %q: %w", name, err)
	}
	if err := runStop(store, name, "TERM", 5); err != nil {
		return fmt.Errorf("create %q: provisioned but failed to stop: %w", name, err)
	}
	fmt.Printf("%s (created, stopped)\n", name)
	return nil
}
