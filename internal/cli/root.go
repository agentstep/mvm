package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

var (
	verbose bool
	mvmDir  string
)

func Execute(version, commit, date string) error {
	rootCmd := newRootCmd(version, commit, date)
	return rootCmd.Execute()
}

func newRootCmd(version, commit, date string) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "mvm",
		Short: "Run Firecracker microVMs on macOS",
		Long:  "mvm makes it trivially easy to run Firecracker microVMs on macOS Apple Silicon via Lima.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")

	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	mvmDir = filepath.Join(home, ".mvm")

	limaClient := lima.NewClient("mvm")
	store := state.NewStore(filepath.Join(mvmDir, "state.json"))

	rootCmd.AddCommand(
		newVersionCmd(version, commit, date),
		newInitCmd(limaClient, store),
		newStartCmd(store),
		newStopCmd(store),
		newPauseCmd(store),
		newResumeCmd(store),
		newSSHCmd(store),
		newExecCmd(store),
		newLogsCmd(limaClient, store),
		newListCmd(),
		newDeleteCmd(),
		newPoolCmd(),
		newDoctorCmd(limaClient, store),
		newUpdateCmd(version),
		newDiffCmd(limaClient, store),
		newTemplateCmd(limaClient, store),
		newSnapshotCmd(),
		newIdleCmd(limaClient, store),
		newInstallCmd(limaClient, store),
		newServeCmd(limaClient, store),
		newMenuCmd(),
	)

	return rootCmd
}

func newVersionCmd(version, commit, date string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print mvm version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("mvm %s (commit: %s, built: %s)\n", version, commit, date)
		},
	}
}
