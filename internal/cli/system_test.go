package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func testSystemCmd(t *testing.T) *cobra.Command {
	t.Helper()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	return newSystemCmd(lima.NewClient("mvm"), store, "1.2.3", "abc123", "2026-07-21")
}

func TestSystemSubcommandsRegistered(t *testing.T) {
	c := testSystemCmd(t)
	if c.Use != "system" {
		t.Fatalf("Use = %q, want system", c.Use)
	}
	have := map[string]bool{}
	for _, sub := range c.Commands() {
		have[sub.Name()] = true
	}
	// Task 17 wires these; status/df are added by Tasks 18/19 (which
	// extend this test to assert them).
	for _, w := range []string{"version", "logs", "start", "stop", "install", "uninstall"} {
		if !have[w] {
			t.Errorf("missing subcommand %q (have %v)", w, have)
		}
	}
}

func TestSystemVersionPrints(t *testing.T) {
	c := testSystemCmd(t)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{"version"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "1.2.3") {
		t.Errorf("output %q missing version 1.2.3", buf.String())
	}
}
