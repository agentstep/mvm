package cli

import "testing"

// TestSplitVMPath pins how a VM reference is distinguished from a local path.
// Getting this wrong writes a host file where a guest file was meant, or the
// reverse.
func TestSplitVMPath(t *testing.T) {
	cases := []struct {
		in       string
		wantVM   string
		wantPath string
		wantOK   bool
	}{
		{"web:/etc/hosts", "web", "/etc/hosts", true},
		{"my-vm:/a/b/c", "my-vm", "/a/b/c", true},
		{"./local.txt", "", "", false},
		{"/absolute/local", "", "", false},
		{"relative.txt", "", "", false},
		// A colon with a relative remainder is a filename, not a VM reference —
		// otherwise "C:\tmp\x" or "notes:draft" would be read as a VM path.
		{"C:\\tmp\\x", "", "", false},
		{"notes:draft", "", "", false},
		{":/no/vm/name", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		vm, path, ok := splitVMPath(c.in)
		if ok != c.wantOK || vm != c.wantVM || path != c.wantPath {
			t.Errorf("splitVMPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, vm, path, ok, c.wantVM, c.wantPath, c.wantOK)
		}
	}
}
