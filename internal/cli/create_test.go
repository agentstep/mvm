package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func TestRunCreateRejectsExistingName(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	if err := store.AddVM(&state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddVM: %v", err)
	}
	err := runCreate(store, "box", "base", 0, 0, "open", nil, nil, "", nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runCreate() = %v, want an \"already exists\" error", err)
	}
}

func TestNewCreateCmdFlags(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	cmd := newCreateCmd(store)
	for _, f := range []struct{ long, short string }{
		{"cpus", "c"}, {"memory", "m"}, {"volume", "v"}, {"publish", "p"},
	} {
		fl := cmd.Flags().Lookup(f.long)
		if fl == nil {
			t.Errorf("--%s not registered", f.long)
			continue
		}
		if fl.Shorthand != f.short {
			t.Errorf("--%s shorthand = %q, want %q", f.long, fl.Shorthand, f.short)
		}
	}
	if cmd.Flags().Lookup("image") == nil {
		t.Error("--image not registered")
	}
}
