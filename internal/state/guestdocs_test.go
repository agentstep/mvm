package state

import (
	"strings"
	"testing"
)

// TestGuestDocsHaveNoApostrophe guards the same class of failure that has
// already cost one full rootfs build: the docs are written via a single-quoted
// heredoc inside the build script, so one apostrophe closes the quote and the
// symptom surfaces far from the cause.
func TestGuestDocsHaveNoApostrophe(t *testing.T) {
	if i := strings.IndexByte(GuestDocs, '\''); i >= 0 {
		line := 1 + strings.Count(GuestDocs[:i], "\n")
		t.Errorf("apostrophe at line %d will terminate the quoted heredoc in the rootfs build", line)
	}
}

// TestGuestDocsCoverTheCapabilities pins that the doc actually describes what
// makes this sandbox different. Without these an agent treats the VM as an
// ordinary Linux box and never uses the features that let it recover from its
// own mistakes.
func TestGuestDocsCoverTheCapabilities(t *testing.T) {
	for _, topic := range []string{"snapshot", "service", "bounce", "egress policy"} {
		if !strings.Contains(strings.ToLower(GuestDocs), topic) {
			t.Errorf("guest docs never mention %q", topic)
		}
	}
}

// It must be short: this lands in the context window of every agent that runs
// in the sandbox, so length is a real cost paid on every single run.
func TestGuestDocsStayShort(t *testing.T) {
	if n := len(GuestDocs); n > 3000 {
		t.Errorf("guest docs are %d bytes; keep under 3000 — this is in every agent's context", n)
	}
}

func TestGuestDocsPathsAreAbsolute(t *testing.T) {
	for _, p := range []string{GuestDocsPath, GuestDocsAlias} {
		if !strings.HasPrefix(p, "/") {
			t.Errorf("%q must be an absolute path", p)
		}
	}
}
