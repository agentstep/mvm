package cli

import (
	"strings"
	"testing"
)

// A threshold the sweep would silently discard must fail the command instead.
//
// parseThreshold in the daemon treats anything unparseable as "no threshold" — correct there (a VM
// nobody opted in must never be archived), wrong at the CLI, where it would leave the operator with
// a VM they believe is cost-controlled and a command that exited 0.
func TestParseTieringRejectsUnusableDurations(t *testing.T) {
	for _, tc := range []struct{ idle, archive string }{
		{"5min", ""},
		{"soon", ""},
		{"-5m", ""},
		{"0s", ""},
		{"", "1 hour"},
		{"", "-1h"},
		{"", "0"},
	} {
		got, err := parseTiering(tc.idle, tc.archive)
		if err == nil {
			t.Errorf("parseTiering(%q, %q) accepted a duration the sweep would discard", tc.idle, tc.archive)
		}
		if got != nil {
			t.Errorf("parseTiering(%q, %q) returned a spec alongside its error", tc.idle, tc.archive)
		}
	}
}

// No flags means no tiering, and that must stay the default — a VM nobody opted in is never
// paused or archived.
func TestParseTieringDefaultsToDisabled(t *testing.T) {
	got, err := parseTiering("", "")
	if err != nil {
		t.Fatalf("unset flags should not error: %v", err)
	}
	if got != nil {
		t.Fatalf("unset flags produced a tiering spec %+v — VMs would tier without opting in", got)
	}
}

func TestParseTieringCarriesValidThresholds(t *testing.T) {
	got, err := parseTiering("5m", "1h")
	if err != nil {
		t.Fatalf("valid durations rejected: %v", err)
	}
	if got == nil || got.IdleTimeout != "5m" || got.ArchiveAfter != "1h" {
		t.Fatalf("thresholds not carried: %+v", got)
	}

	// One without the other is legitimate: pause but never archive.
	got, err = parseTiering("5m", "")
	if err != nil || got == nil || got.IdleTimeout != "5m" || got.ArchiveAfter != "" {
		t.Fatalf("idle-only spec = %+v, err = %v", got, err)
	}
}

// The error must name the flag the operator typed, not the internal field.
func TestParseTieringNamesTheFlag(t *testing.T) {
	_, err := parseTiering("5min", "")
	if err == nil || !strings.Contains(err.Error(), "--idle-timeout") {
		t.Fatalf("error should name --idle-timeout, got %v", err)
	}
	_, err = parseTiering("", "1 hour")
	if err == nil || !strings.Contains(err.Error(), "--archive-after") {
		t.Fatalf("error should name --archive-after, got %v", err)
	}
}
