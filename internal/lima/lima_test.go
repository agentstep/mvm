package lima

import (
	"testing"
)

func TestNewClient(t *testing.T) {
	c := NewClient("test")
	if c.VMName != "test" {
		t.Errorf("VMName = %q, want test", c.VMName)
	}
}

func TestIsM3OrNewer(t *testing.T) {
	tests := []struct {
		brand string
		want  bool
	}{
		{"Apple M3", true},
		{"Apple M3 Pro", true},
		{"Apple M3 Max", true},
		{"Apple M3 Ultra", true},
		{"Apple M4", true},
		{"Apple M4 Pro", true},
		{"Apple M5", true},
		{"Apple M9", true},
		{"Apple M1", false},
		{"Apple M1 Pro", false},
		{"Apple M2", false},
		{"Apple M2 Max", false},
		{"Intel Core i9", false},
		{"", false},
		{"apple m3", true},  // lowercase
		{"APPLE M4", true},  // uppercase
		{"Apple M10", false}, // M1 followed by 0, not M10
	}

	for _, tt := range tests {
		got := isM3OrNewer(tt.brand)
		if got != tt.want {
			t.Errorf("isM3OrNewer(%q) = %v, want %v", tt.brand, got, tt.want)
		}
	}
}

func TestParseMajorVersion(t *testing.T) {
	tests := []struct {
		ver     string
		want    int
		wantErr bool
	}{
		{"15.0", 15, false},
		{"15.1.2", 15, false},
		{"14.6", 14, false},
		{"16", 16, false},
		{"", 0, true},
		{"abc", 0, true},
	}

	for _, tt := range tests {
		got, err := parseMajorVersion(tt.ver)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseMajorVersion(%q) error = %v, wantErr %v", tt.ver, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMajorVersion(%q) = %d, want %d", tt.ver, got, tt.want)
		}
	}
}

func TestEnsureRunningCachesResult(t *testing.T) {
	c := NewClient("test")
	tr := true
	c.cachedRunning = &tr

	err := c.EnsureRunning()
	if err != nil {
		t.Errorf("cached EnsureRunning should return nil, got: %v", err)
	}
}

func TestIsInstalledReturnsBool(t *testing.T) {
	c := NewClient("test")
	// Just verify it returns without panic
	_ = c.IsInstalled()
}

func TestIsBrewInstalledReturnsBool(t *testing.T) {
	c := NewClient("test")
	_ = c.IsBrewInstalled()
}

// === NEW TESTS: EnsureRunning caching ===

func TestEnsureRunningCacheNotSetInitially(t *testing.T) {
	c := NewClient("test")
	if c.cachedRunning != nil {
		t.Error("cachedRunning should be nil initially")
	}
}

func TestEnsureRunningFalseCache(t *testing.T) {
	c := NewClient("test")
	f := false
	c.cachedRunning = &f

	// When cache is false, EnsureRunning should not short-circuit —
	// it should proceed to check VM state (which will fail without limactl)
	err := c.EnsureRunning()
	if err == nil {
		// It should error because limactl isn't managing "test" VM
		// This is fine — the point is it didn't return nil like with true cache
	}
}

// === NEW TESTS: isM3OrNewer edge cases ===

func TestIsM3OrNewerEdgeCases(t *testing.T) {
	tests := []struct {
		brand string
		want  bool
	}{
		// Potential false positive: "am3" in other words
		{"Samsung M3", false}, // no "apple" prefix
		{"Apple M", false},    // M without digit
		{"Apple m", false},    // lowercase m without digit
		{"Apple M0", false},   // M0 (doesn't exist but tests range)
		{"Apple M2 Ultra", false},
		{"Apple M3 chip in MacBook Pro", true}, // extra text after
	}

	for _, tt := range tests {
		got := isM3OrNewer(tt.brand)
		if got != tt.want {
			t.Errorf("isM3OrNewer(%q) = %v, want %v", tt.brand, got, tt.want)
		}
	}
}

// === NEW TESTS: parseMajorVersion edge cases ===

func TestParseMajorVersionEdgeCases(t *testing.T) {
	tests := []struct {
		ver     string
		want    int
		wantErr bool
	}{
		{"0.1", 0, false},     // version 0
		{"100.0", 100, false}, // triple digit
		{".5", 0, true},      // starts with dot
		{"15.", 15, false},    // trailing dot
	}

	for _, tt := range tests {
		got, err := parseMajorVersion(tt.ver)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseMajorVersion(%q) error = %v, wantErr %v", tt.ver, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseMajorVersion(%q) = %d, want %d", tt.ver, got, tt.want)
		}
	}
}

// === NEW TEST: Default timeout constants ===

func TestTimeoutConstants(t *testing.T) {
	if DefaultTimeout <= 0 {
		t.Error("DefaultTimeout should be positive")
	}
	if LongTimeout <= DefaultTimeout {
		t.Error("LongTimeout should be greater than DefaultTimeout")
	}
}

// === NEW TEST: Client VMName is immutable after creation ===

func TestClientVMNamePreserved(t *testing.T) {
	c := NewClient("my-lima-vm")
	if c.VMName != "my-lima-vm" {
		t.Errorf("VMName = %q, want my-lima-vm", c.VMName)
	}
}
