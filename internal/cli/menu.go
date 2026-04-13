package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
)

func newMenuCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "menu",
		Short: "Open the macOS menu bar status icon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return launchMenuBar()
		},
	}
}

func launchMenuBar() error {
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "mvm-menu")
		if _, err := os.Stat(candidate); err == nil {
			return syscall.Exec(candidate, []string{"mvm-menu"}, os.Environ())
		}
	}

	path, err := exec.LookPath("mvm-menu")
	if err != nil {
		return fmt.Errorf("mvm-menu not found. Build it with: make menu")
	}
	return syscall.Exec(path, []string{"mvm-menu"}, os.Environ())
}
