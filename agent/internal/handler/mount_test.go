package handler

import (
	"strings"
	"testing"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

func TestValidateMountRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name string
		req  *protocol.MountRequest
		want string
	}{
		{"nil", nil, "missing"},
		{"no target", &protocol.MountRequest{Source: "vol0", FSType: "virtiofs"}, "target"},
		{"relative target", &protocol.MountRequest{Source: "vol0", Target: "data", FSType: "virtiofs"}, "absolute"},
		{"no fstype", &protocol.MountRequest{Source: "vol0", Target: "/data"}, "fstype"},
		{"no source", &protocol.MountRequest{Target: "/data", FSType: "virtiofs"}, "source"},
	}
	for _, c := range cases {
		err := ValidateMount(c.req)
		if err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), c.want) {
			t.Errorf("%s: error %q should mention %q", c.name, err, c.want)
		}
	}
}

// TestValidateMountRejectsTraversal covers the path that made the old
// exec-a-shell-string approach risky: Target is used as a mount point, so a
// traversal or a parent reference could place a filesystem somewhere the caller
// never named.
func TestValidateMountRejectsTraversal(t *testing.T) {
	for _, target := range []string{
		"/data/../../etc",
		"/data/..",
		"/../etc",
	} {
		req := &protocol.MountRequest{Source: "vol0", Target: target, FSType: "virtiofs"}
		if err := ValidateMount(req); err == nil {
			t.Errorf("ValidateMount(target=%q) should have been rejected", target)
		}
	}
}

func TestValidateMountAcceptsGoodRequest(t *testing.T) {
	req := &protocol.MountRequest{
		Source: "vol0", Target: "/workspace", FSType: "virtiofs", MkDir: true,
	}
	if err := ValidateMount(req); err != nil {
		t.Errorf("ValidateMount rejected a valid request: %v", err)
	}
}

// TestValidateMountAllowsCleanNestedPaths guards against over-strict
// validation: a legitimate deep mount point must still be accepted.
func TestValidateMountAllowsCleanNestedPaths(t *testing.T) {
	for _, target := range []string{"/a/b/c", "/workspace/data", "/mnt/vol0"} {
		req := &protocol.MountRequest{Source: "vol0", Target: target, FSType: "virtiofs"}
		if err := ValidateMount(req); err != nil {
			t.Errorf("ValidateMount(target=%q) = %v, want accepted", target, err)
		}
	}
}
