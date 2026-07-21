package cli

import "testing"

func TestVerboseHasNoShorthand(t *testing.T) {
	cmd := newRootCmd("test", "test", "test")
	f := cmd.PersistentFlags().Lookup("verbose")
	if f == nil {
		t.Fatal("--verbose flag missing")
	}
	if f.Shorthand != "" {
		t.Errorf("--verbose shorthand = %q, want none (freed for --volume)", f.Shorthand)
	}
}
