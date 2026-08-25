package cli

import (
	"fmt"
	"time"

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

		idleTimeout  string
		archiveAfter string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Provision a microVM and leave it stopped",
		Long: `Provision a microVM (allocate config, prepare rootfs) and leave it stopped.

The VM is booted once to lay down its rootfs and network allocation, then
stopped — start it later with: mvm start <name>.

  mvm create mybox
  mvm create mybox --image my-image -c 4 -m 2048
  mvm create web -p 8080:80 -v ./src:/app
  mvm create box --idle-timeout 5m --archive-after 1h

Idle tiering (--idle-timeout, --archive-after) is applied by the mvm daemon's
sweep, so it takes effect only while the daemon is running, and only on the
firecracker backend.`,
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
			tiering, err := parseTiering(idleTimeout, archiveAfter)
			if err != nil {
				return err
			}
			return runCreate(store, args[0], image, cpus, memoryMB, netPolicy, portMaps, vols, seccomp, tiering)
		},
	}

	cmd.Flags().StringVar(&image, "image", "", "custom rootfs image name; \"base\" or empty = default rootfs")
	cmd.Flags().IntVarP(&cpus, "cpus", "c", 0, "vCPU count (default: 2)")
	cmd.Flags().IntVarP(&memoryMB, "memory", "m", 0, "RAM in MiB (default: 1024)")
	cmd.Flags().StringVar(&netPolicy, "net-policy", "open", "network policy: open, deny, or allow:domain1,domain2")
	cmd.Flags().StringArrayVarP(&ports, "publish", "p", nil, "publish port (hostPort:guestPort[/proto])")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "v", nil, "bind mount (hostPath:guestPath)")
	cmd.Flags().StringVar(&seccomp, "seccomp", "", "seccomp profile: strict, moderate, or permissive")
	cmd.Flags().StringVar(&idleTimeout, "idle-timeout", "", "pause the VM after this much idleness (e.g. 5m); unset = never")
	cmd.Flags().StringVar(&archiveAfter, "archive-after", "", "snapshot and stop the VM after this much idleness (e.g. 1h); unset = never")

	return cmd
}

// runCreate provisions a VM and leaves it stopped. There is no
// create-without-boot path on either backend, so create honestly boots then
// stops — the rootfs clone and net allocation persist, and the VM is parked
// "stopped" ready for `mvm start`.
// parseTiering validates the thresholds up front so a typo fails the command rather than being
// silently discarded by the sweep — which treats anything unparseable as "no threshold" and would
// leave the caller with a VM they believe is cost-controlled and isn't.
func parseTiering(idleTimeout, archiveAfter string) (*TieringSpec, error) {
	for _, f := range []struct{ flag, val string }{
		{"--idle-timeout", idleTimeout},
		{"--archive-after", archiveAfter},
	} {
		if f.val == "" {
			continue
		}
		d, err := time.ParseDuration(f.val)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid %s %q: want a positive duration such as 5m or 1h", f.flag, f.val)
		}
	}
	if idleTimeout == "" && archiveAfter == "" {
		return nil, nil
	}
	return &TieringSpec{IdleTimeout: idleTimeout, ArchiveAfter: archiveAfter}, nil
}

func runCreate(store *state.Store, name, image string, cpus, memoryMB int, netPolicy string, ports []state.PortMap, volumes []string, seccomp string, tiering *TieringSpec) error {
	existing, err := existingVMNames(store)
	if err != nil {
		return err
	}
	if existing[name] {
		return fmt.Errorf("microVM %q already exists", name)
	}
	if err := runStart(store, name, true, ports, netPolicy, volumes, seccomp, "", cpus, memoryMB, resolveImage(image), false, nil, nil, true, tiering); err != nil {
		return fmt.Errorf("create %q: %w", name, err)
	}
	if err := runStop(store, name, "TERM", 5); err != nil {
		return fmt.Errorf("create %q: provisioned but failed to stop: %w", name, err)
	}
	fmt.Printf("%s (created, stopped)\n", name)
	return nil
}
