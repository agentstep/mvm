package state

import "testing"

func TestParseNetPolicyModes(t *testing.T) {
	tests := []struct {
		in       string
		wantMode NetPolicyMode
		wantDoms []string
	}{
		{"", NetPolicyOpen, nil},
		{"open", NetPolicyOpen, nil},
		{"deny", NetPolicyDeny, nil},
		{"allow:github.com", NetPolicyAllow, []string{"github.com"}},
		{"allow:github.com,registry.npmjs.org", NetPolicyAllow, []string{"github.com", "registry.npmjs.org"}},
		{"allow: github.com , NPMJS.org ", NetPolicyAllow, []string{"github.com", "npmjs.org"}},
	}
	for _, tt := range tests {
		got, err := ParseNetPolicy(tt.in)
		if err != nil {
			t.Errorf("ParseNetPolicy(%q) returned error: %v", tt.in, err)
			continue
		}
		if got.Mode != tt.wantMode {
			t.Errorf("ParseNetPolicy(%q).Mode = %v, want %v", tt.in, got.Mode, tt.wantMode)
		}
		if len(got.Domains) != len(tt.wantDoms) {
			t.Errorf("ParseNetPolicy(%q).Domains = %v, want %v", tt.in, got.Domains, tt.wantDoms)
			continue
		}
		for i := range tt.wantDoms {
			if got.Domains[i] != tt.wantDoms[i] {
				t.Errorf("ParseNetPolicy(%q).Domains[%d] = %q, want %q", tt.in, i, got.Domains[i], tt.wantDoms[i])
			}
		}
	}
}

// TestParseNetPolicyRejectsShellMetacharacters is the load-bearing test.
// Domains flow into ipset/DNS handling; anything that could terminate a shell
// word must be refused at the parser, not defended against downstream.
func TestParseNetPolicyRejectsShellMetacharacters(t *testing.T) {
	bad := []string{
		"allow:$(rm -rf /)",
		"allow:`whoami`",
		"allow:github.com; rm -rf /",
		"allow:github.com && evil.com",
		"allow:github.com|evil.com",
		"allow:evil.com\nrm -rf /",
		"allow:-leadinghyphen.com",
		"allow:trailinghyphen-.com",
		"allow:double..dot.com",
		"allow:.leadingdot.com",
		"allow:has_underscore.com",
		"allow:has space.com",
	}
	for _, in := range bad {
		if _, err := ParseNetPolicy(in); err == nil {
			t.Errorf("ParseNetPolicy(%q) should have been rejected", in)
		}
	}
}

func TestParseNetPolicyRejectsEmptyAllowList(t *testing.T) {
	for _, in := range []string{"allow:", "allow:,", "allow: , "} {
		if _, err := ParseNetPolicy(in); err == nil {
			t.Errorf("ParseNetPolicy(%q) should have been rejected (no domains)", in)
		}
	}
}

func TestParseNetPolicyRejectsUnknownMode(t *testing.T) {
	for _, in := range []string{"denyall", "allow", "block", "ALLOW:github.com"} {
		if _, err := ParseNetPolicy(in); err == nil {
			t.Errorf("ParseNetPolicy(%q) should have been rejected", in)
		}
	}
}

func TestParseNetPolicyRejectsOverlongNames(t *testing.T) {
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := ParseNetPolicy("allow:" + string(long) + ".com"); err == nil {
		t.Error("a 64-character label should have been rejected (max 63)")
	}
}

func TestEgressHostPackages(t *testing.T) {
	pkgs := EgressHostPackages()
	for _, want := range []string{"ipset", "iptables", "conntrack"} {
		found := false
		for _, p := range pkgs {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("EgressHostPackages() missing %q, got %v", want, pkgs)
		}
	}
}
