package cli

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestParseSignal(t *testing.T) {
	cases := []struct {
		in   string
		want syscall.Signal
	}{
		{"KILL", syscall.SIGKILL}, {"SIGKILL", syscall.SIGKILL}, {"9", syscall.SIGKILL},
		{"TERM", syscall.SIGTERM}, {"sigterm", syscall.SIGTERM}, {"INT", syscall.SIGINT},
		{"bogus", syscall.SIGKILL},
	}
	for _, c := range cases {
		if got := parseSignal(c.in); got != c.want {
			t.Errorf("parseSignal(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRunKillAppleVZNotRunning(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	if err := store.AddVM(&state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}
	err := runKill(store, "box", "KILL")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("runKill() = %v, want a \"not running\" error", err)
	}
}

func TestNewKillCmdFlags(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newKillCmd(store)
	sig := cmd.Flags().Lookup("signal")
	if sig == nil || sig.Shorthand != "s" || sig.DefValue != "KILL" {
		t.Errorf("--signal = %+v, want shorthand s default KILL", sig)
	}
	all := cmd.Flags().Lookup("all")
	if all == nil || all.Shorthand != "a" {
		t.Errorf("--all = %+v, want shorthand a", all)
	}
}
