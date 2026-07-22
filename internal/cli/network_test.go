package cli

import (
	"path/filepath"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

func TestNetworkCmdWiring(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	c := newNetworkCmd(store)
	if c.Use != "network" {
		t.Fatalf("Use = %q, want network", c.Use)
	}
	names := map[string]bool{}
	for _, sub := range c.Commands() {
		names[sub.Name()] = true
	}
	if !names["ls"] || !names["inspect"] {
		t.Fatalf("subcommands = %v, want ls+inspect", names)
	}
	ls, _, _ := c.Find([]string{"ls"})
	if ls.Flags().Lookup("format") == nil {
		t.Error("ls missing --format")
	}
}

func TestNetworkInspectUnknownErrors(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")
	if err := runNetworkInspect(store, "nope"); err == nil {
		t.Error("inspect of a non-default network should error (only 'default' exists)")
	}
	if err := runNetworkInspect(store, "default"); err != nil {
		t.Errorf("inspect default: %v", err)
	}
}
