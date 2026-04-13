package cli

import (
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newDiffCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <name>",
		Short: "Show filesystem changes made inside a VM",
		Long: `Show files created, modified, or deleted inside a VM since boot.

  mvm diff my-app`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			findCmd := `find / -xdev -newer /proc/1/cmdline -not -path '/proc/*' -not -path '/sys/*' -not -path '/dev/*' -not -path '/run/*' -not -path '/tmp/*' -ls 2>/dev/null | head -100`
			return runExec(limaClient, store, args[0], []string{"sh", "-c", findCmd}, false, "", nil, "")
		},
	}
	return cmd
}
