package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestSignalIsKill(t *testing.T) {
	cases := map[string]bool{
		"KILL": true, "SIGKILL": true, "9": true, "kill": true,
		"TERM": false, "SIGTERM": false, "15": false, "": false,
	}
	for in, want := range cases {
		if got := signalIsKill(in); got != want {
			t.Errorf("signalIsKill(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewStopCmdFlags(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newStopCmd(store)
	sig := cmd.Flags().Lookup("signal")
	if sig == nil || sig.Shorthand != "s" || sig.DefValue != "TERM" {
		t.Errorf("--signal = %+v, want shorthand s default TERM", sig)
	}
	tm := cmd.Flags().Lookup("time")
	if tm == nil || tm.Shorthand != "t" || tm.DefValue != "5" {
		t.Errorf("--time = %+v, want shorthand t default 5", tm)
	}
	all := cmd.Flags().Lookup("all")
	if all == nil || all.Shorthand != "a" {
		t.Errorf("--all = %+v, want shorthand a", all)
	}
	if cmd.Flags().Lookup("force") != nil {
		t.Error("--force should be gone from stop (use -s KILL)")
	}
}

func TestRunStopAppleVZNotRunning(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	if err := store.AddVM(&state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}
	if err := runStop(store, "box", "TERM", 5); err == nil {
		t.Fatal("runStop() = nil, want a \"not running\" error")
	}
}
