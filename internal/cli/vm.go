package cli

import (
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

// newVMCmd groups the single-VM lifecycle verbs under one noun, mirroring
// the `<tool> <noun> <verb>` shape used by container/docker (e.g.
// `container image ...`) — step 5 of
// docs/superpowers/specs/2026-07-19-image-vm-organization-design.md. Every
// verb here also stays available at the top level as a permanent
// shortcut; this is purely an additional, organized entry point, not a
// deprecation. Each child is constructed fresh from the same function the
// top-level registration uses, so the two trees never share flag state.
func newVMCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Manage individual microVMs (start, stop, exec, ...)",
	}

	cmd.AddCommand(
		newStartCmd(store),
		newRunCmd(store),
		newStopCmd(store),
		newPauseCmd(store),
		newResumeCmd(store),
		newSSHCmd(store),
		newExecCmd(store),
		newLogsCmd(limaClient, store),
		newListCmd(store),
		newInspectCmd(store),
		newDeleteCmd(store),
	)

	return cmd
}
