package cli

import (
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newSSHCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <name> [-- <command>]",
		Short: "Open a shell in a running microVM",
		Long: `Open a shell inside a running microVM, or run a command:

  mvm ssh my-vm
  mvm ssh my-vm -- uname -a`,
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			remoteArgs := []string{"sh"}
			for i, arg := range args {
				if arg == "--" {
					remoteArgs = args[i+1:]
					break
				}
			}
			// Delegate to exec with interactive flag
			return runExec(store, name, remoteArgs, true, "", nil, "")
		},
	}
}
