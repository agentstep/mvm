package cli

import (
	"path/filepath"
	"testing"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func TestNewVMCmdGroupsLifecycleVerbs(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	limaClient := lima.NewClient("mvm-test")

	cmd := newVMCmd(limaClient, store)

	if cmd.Use != "vm" {
		t.Errorf("Use = %q, want vm", cmd.Use)
	}

	// canonical verb -> accepted names (including aliases) that must
	// resolve to a child command under `mvm vm`.
	wantChildren := map[string][]string{
		"start":   {"start"},
		"run":     {"run"},
		"stop":    {"stop"},
		"pause":   {"pause"},
		"resume":  {"resume"},
		"ssh":     {"ssh"},
		"exec":    {"exec"},
		"logs":    {"logs"},
		"list":    {"list", "ls"},
		"inspect": {"inspect"},
		"delete":  {"delete", "rm"},
	}

	if got, want := len(cmd.Commands()), len(wantChildren); got != want {
		t.Errorf("len(cmd.Commands()) = %d, want %d", got, want)
	}

	for canonical, names := range wantChildren {
		for _, name := range names {
			if findChild(cmd, name) == nil {
				t.Errorf("mvm vm %s: no child command resolves %q", canonical, name)
			}
		}
	}
}

// TestNewVMCmdChildrenAreIndependentInstances confirms the vm-nested
// commands are fresh *cobra.Command values, not the same objects the
// top-level registration uses — required so flag state never bleeds
// between `mvm start` and `mvm vm start`.
func TestNewVMCmdChildrenAreIndependentInstances(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	topLevel := newStartCmd(store)
	nested := newVMCmd(lima.NewClient("mvm-test"), store)

	nestedStart := findChild(nested, "start")
	if nestedStart == nil {
		t.Fatal("mvm vm start: not found")
	}
	if nestedStart == topLevel {
		t.Error("mvm vm start shares the same *cobra.Command instance as top-level mvm start")
	}
}

// findChild returns the first child of cmd matching name by Name() or an
// alias, or nil if none match.
func findChild(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name {
			return c
		}
		for _, a := range c.Aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}
