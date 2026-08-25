package server

import (
	"testing"
	"time"
)

// parseThreshold decides whether a tier applies at all. It returning a default on bad input is how
// a VM nobody opted in gets archived, so the disabled cases matter more than the parsing.
func TestParseThresholdDisablesRatherThanDefaulting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  time.Duration
		valid bool
	}{
		{"empty disables", "", 0, false},
		{"garbage disables", "soon", 0, false},
		{"zero disables", "0s", 0, false},
		{"negative disables", "-5m", 0, false},
		{"minutes", "5m", 5 * time.Minute, true},
		{"hours", "1h", time.Hour, true},
		{"seconds", "90s", 90 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseThreshold(tc.in)
			if ok != tc.valid {
				t.Fatalf("parseThreshold(%q) valid = %v, want %v", tc.in, ok, tc.valid)
			}
			if ok && got != tc.want {
				t.Fatalf("parseThreshold(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The two thresholds are independent: a VM may pause without ever archiving, which is the common
// case for a sandbox in active multi-turn use.
func TestThresholdsAreIndependent(t *testing.T) {
	if _, ok := parseThreshold("5m"); !ok {
		t.Fatal("idle timeout should parse")
	}
	if _, ok := parseThreshold(""); ok {
		t.Fatal("an unset archive threshold must not enable archiving")
	}
}
