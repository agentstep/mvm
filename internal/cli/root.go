package cli

import (
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

var (
	remoteFlag string
	apiKeyFlag string
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
		Use:           "mvm",
		Short:         "Run Firecracker microVMs on macOS",
		Long:          "mvm makes it trivially easy to run Firecracker microVMs on macOS Apple Silicon via Lima.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "verbose output")
	rootCmd.PersistentFlags().StringVar(&remoteFlag, "remote", "", "remote daemon URL (e.g. https://server:19876)")
	rootCmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key for remote daemon")

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if remoteFlag != "" {
			os.Setenv("MVM_REMOTE", remoteFlag)
		}
		if apiKeyFlag != "" {
			os.Setenv("MVM_API_KEY", apiKeyFlag)
		}
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	mvmDir = filepath.Join(home, ".mvm")

	limaClient := lima.NewClient("mvm")
	store := state.NewStore(filepath.Join(mvmDir, "state.json"))

	rootCmd.AddCommand(
		newSystemCmd(limaClient, store, version, commit, date),
		newInitCmd(limaClient, store),
		newStartCmd(store),
		newCreateCmd(store),
		newRunCmd(store),
		newVMCmd(limaClient, store),
		newStopCmd(store),
		newKillCmd(store),
		newPauseCmd(store),
		newResumeCmd(store),
		newSSHCmd(store),
		newExecCmd(store),
		newLogsCmd(store),
		newListCmd(store),
		newStatsCmd(store),
		newInspectCmd(store),
		newDeleteCmd(store),
		newPoolCmd(),
		newUpdateCmd(version),
		newDiffCmd(limaClient, store),
		newTemplateCmd(limaClient, store),
		newSnapshotCmd(store),
		newBenchCmd(store),
		newSecretCmd(),
		newPreviewCmd(store),
		newBounceCmd(store),
		newServiceCmd(store),
		newCpCmd(store),
		newDirCmd(store),
		newBuildCmd(store),
		newImageCmd(store),
		newNetworkCmd(store),
		newIdleCmd(limaClient, store),
		newInstallCmd(limaClient, store),
		newMenuCmd(),
		newForwardDaemonCmd(store),
	)

	return rootCmd
}
