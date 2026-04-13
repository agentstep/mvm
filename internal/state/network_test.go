package state

import (
	"net"
	"strings"
	"testing"
)

func TestAllocateNet(t *testing.T) {
	tests := []struct {
		index    int
		tapDev   string
		tapIP    string
		guestIP  string
		subnet   string
		guestMAC string
	}{
		{0, "tap0", "172.16.0.1", "172.16.0.2", "172.16.0.0/30", "06:00:AC:10:00:02"},
		{1, "tap1", "172.16.0.5", "172.16.0.6", "172.16.0.4/30", "06:00:AC:10:00:06"},
		{2, "tap2", "172.16.0.9", "172.16.0.10", "172.16.0.8/30", "06:00:AC:10:00:0A"},
		{10, "tap10", "172.16.0.41", "172.16.0.42", "172.16.0.40/30", "06:00:AC:10:00:2A"},
		{62, "tap62", "172.16.0.249", "172.16.0.250", "172.16.0.248/30", "06:00:AC:10:00:FA"},
	}

	for _, tt := range tests {
		alloc := AllocateNet(tt.index)
		if alloc.TAPDev != tt.tapDev {
			t.Errorf("index %d: TAPDev = %q, want %q", tt.index, alloc.TAPDev, tt.tapDev)
		}
		if alloc.TAPIP != tt.tapIP {
			t.Errorf("index %d: TAPIP = %q, want %q", tt.index, alloc.TAPIP, tt.tapIP)
		}
		if alloc.GuestIP != tt.guestIP {
			t.Errorf("index %d: GuestIP = %q, want %q", tt.index, alloc.GuestIP, tt.guestIP)
		}
		if alloc.Subnet != tt.subnet {
			t.Errorf("index %d: Subnet = %q, want %q", tt.index, alloc.Subnet, tt.subnet)
		}
		if alloc.GuestMAC != tt.guestMAC {
			t.Errorf("index %d: GuestMAC = %q, want %q", tt.index, alloc.GuestMAC, tt.guestMAC)
		}
		if alloc.Index != tt.index {
			t.Errorf("index %d: Index = %d", tt.index, alloc.Index)
		}
	}
}

func TestAllocateNetConsecutive(t *testing.T) {
	// Verify no IP collisions across all 62 slots
	seen := make(map[string]int)
	for i := 0; i < 62; i++ {
		alloc := AllocateNet(i)
		if prev, exists := seen[alloc.GuestIP]; exists {
			t.Errorf("IP collision: index %d and %d both got %s", prev, i, alloc.GuestIP)
		}
		seen[alloc.GuestIP] = i

		if prev, exists := seen[alloc.TAPIP]; exists {
			t.Errorf("TAP IP collision: index %d and %d both got %s", prev, i, alloc.TAPIP)
		}
		seen[alloc.TAPIP] = i

		if prev, exists := seen[alloc.GuestMAC]; exists {
			t.Errorf("MAC collision: index %d and %d both got %s", prev, i, alloc.GuestMAC)
		}
		seen[alloc.GuestMAC] = i
	}
}

func TestSetupTAPScript(t *testing.T) {
	alloc := AllocateNet(0)
	script := SetupTAPScript(alloc)

	if !strings.Contains(script, "tap0") {
		t.Error("should contain tap0")
	}
	if !strings.Contains(script, "172.16.0.1/30") {
		t.Error("should contain gateway CIDR")
	}
	if !strings.Contains(script, "ip tuntap add") {
		t.Error("should create TAP device")
	}
}

func TestDeleteTAPScript(t *testing.T) {
	script := DeleteTAPScript("tap5")
	if !strings.Contains(script, "tap5") {
		t.Error("should contain tap5")
	}
	if !strings.Contains(script, "ip link del") {
		t.Error("should delete link")
	}
}

func TestSetupNATScript(t *testing.T) {
	script := SetupNATScript()
	if !strings.Contains(script, "ip_forward") {
		t.Error("should enable IP forwarding")
	}
	if !strings.Contains(script, "MASQUERADE") {
		t.Error("should set up MASQUERADE")
	}
	if !strings.Contains(script, "FORWARD ACCEPT") {
		t.Error("should accept forwarding")
	}
}

func TestNATSystemdServiceScript(t *testing.T) {
	script := NATSystemdServiceScript()
	if !strings.Contains(script, "mvm-nat.sh") {
		t.Error("should reference NAT script")
	}
	if !strings.Contains(script, "systemctl enable") {
		t.Error("should enable service")
	}
}

// === NEW TESTS: /30 subnet isolation ===

func TestAllocateNetSubnetIsolation(t *testing.T) {
	// Each VM should be in its own /30 subnet — gateway and guest
	// should be in the same /30, but different VMs' guests should NOT overlap
	for i := 0; i < 62; i++ {
		alloc := AllocateNet(i)

		// Parse subnet
		_, ipnet, err := net.ParseCIDR(alloc.Subnet)
		if err != nil {
			t.Fatalf("index %d: invalid subnet CIDR %q: %v", i, alloc.Subnet, err)
		}

		// Both gateway and guest must be within the subnet
		gw := net.ParseIP(alloc.TAPIP)
		guest := net.ParseIP(alloc.GuestIP)

		if !ipnet.Contains(gw) {
			t.Errorf("index %d: gateway %s not in subnet %s", i, alloc.TAPIP, alloc.Subnet)
		}
		if !ipnet.Contains(guest) {
			t.Errorf("index %d: guest %s not in subnet %s", i, alloc.GuestIP, alloc.Subnet)
		}

		// Gateway and guest must be different
		if alloc.TAPIP == alloc.GuestIP {
			t.Errorf("index %d: gateway and guest have same IP %s", i, alloc.TAPIP)
		}
	}
}

// === NEW TEST: MAC address format ===

func TestAllocateNetMACFormat(t *testing.T) {
	for i := 0; i < 62; i++ {
		alloc := AllocateNet(i)

		// MAC should be 06:00:AC:10:XX:XX format (locally administered, unicast)
		if !strings.HasPrefix(alloc.GuestMAC, "06:00:AC:10:") {
			t.Errorf("index %d: MAC %q doesn't have expected prefix", i, alloc.GuestMAC)
		}

		// Should be valid MAC format (6 hex pairs separated by colons)
		parts := strings.Split(alloc.GuestMAC, ":")
		if len(parts) != 6 {
			t.Errorf("index %d: MAC %q should have 6 octets", i, alloc.GuestMAC)
		}
	}
}

// === NEW TEST: ipToMAC with nil IP ===

func TestIPToMACNilIP(t *testing.T) {
	mac := ipToMAC(nil)
	if mac != "06:00:AC:10:00:02" {
		t.Errorf("nil IP MAC = %q, want fallback", mac)
	}
}

// === NEW TEST: TAP device naming ===

func TestAllocateNetTAPDevNaming(t *testing.T) {
	for i := 0; i < 62; i++ {
		alloc := AllocateNet(i)
		expected := strings.Replace("tapN", "N", strings.TrimPrefix(alloc.TAPDev, "tap"), 1)
		if alloc.TAPDev != expected {
			t.Errorf("index %d: TAPDev = %q, want %q", i, alloc.TAPDev, expected)
		}
	}
}

// === NEW TEST: SetupTAPScript cleans up before creating ===

func TestSetupTAPScriptCleansFirst(t *testing.T) {
	alloc := AllocateNet(3)
	script := SetupTAPScript(alloc)

	// Should delete existing TAP before creating new one
	delIdx := strings.Index(script, "ip link del")
	addIdx := strings.Index(script, "ip tuntap add")
	if delIdx < 0 || addIdx < 0 {
		t.Error("script should have both delete and add")
	}
	if delIdx > addIdx {
		t.Error("should delete TAP before creating new one")
	}
}
