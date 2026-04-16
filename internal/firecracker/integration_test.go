package firecracker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

// === Integration: ValidateName + ReserveVM ===

func TestValidateNameAcceptsValidNames(t *testing.T) {
	valid := []string{
		"my-vm",
		"test123",
		"My.VM",
		"a",
		"vm-1_test.2",
		"UPPERCASE",
		"mix.Ed-Case_123",
		"x",
		"vm0",
		"A-B-C",
	}
	for _, name := range valid {
		if err := state.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) should accept: %v", name, err)
		}
	}
}

func TestValidateNameRejectsInjection(t *testing.T) {
	injections := []string{
		"vm; rm -rf /",
		"vm$(whoami)",
		"vm`id`",
		"vm && echo pwned",
		"vm || true",
		"vm\necho injected",
		"vm name with spaces",
		"vm/path/traversal",
		"vm|pipe",
		"vm>redirect",
		"vm<input",
		"vm'quoted",
		`vm"double`,
		"vm$VAR",
		"vm{brace}",
		"vm(paren)",
		"vm[bracket]",
		"vm!bang",
		"vm@at",
		"vm#hash",
		"vm%percent",
		"vm^caret",
		"vm&amp",
		"vm*star",
		"vm=equal",
		"vm+plus",
		"vm~tilde",
		"",
	}
	for _, name := range injections {
		if err := state.ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) should reject", name)
		}
	}
}

// === Integration: AllocateNet + GenerateConfig → valid JSON ===

func TestAllocateNetGenerateConfigProducesValidJSON(t *testing.T) {
	for i := 0; i < 10; i++ {
		alloc := state.AllocateNet(i)
		cfgStr, err := GenerateConfig("integ-vm", alloc, 0, 0)
		if err != nil {
			t.Fatalf("GenerateConfig index %d: %v", i, err)
		}

		// Must be valid JSON
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(cfgStr), &raw); err != nil {
			t.Fatalf("index %d: invalid JSON: %v", i, err)
		}
	}
}

// === Integration: Full config generation → verify all fields ===

func TestFullConfigGenerationAllFields(t *testing.T) {
	alloc := state.AllocateNet(7)
	cfgStr, err := GenerateConfig("full-test", alloc, 8, 4096)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	var cfg fcConfig
	if err := json.Unmarshal([]byte(cfgStr), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Boot source
	if !strings.Contains(cfg.BootSource.KernelImagePath, "vmlinux") {
		t.Error("kernel path should contain vmlinux")
	}
	if !strings.Contains(cfg.BootSource.BootArgs, alloc.GuestIP) {
		t.Errorf("boot args should contain guest IP %s", alloc.GuestIP)
	}
	if !strings.Contains(cfg.BootSource.BootArgs, alloc.TAPIP) {
		t.Errorf("boot args should contain TAP IP %s", alloc.TAPIP)
	}
	if !strings.Contains(cfg.BootSource.BootArgs, "console=ttyS0") {
		t.Error("boot args should have console=ttyS0")
	}

	// Drives
	if len(cfg.Drives) != 1 {
		t.Fatalf("drives len = %d, want 1", len(cfg.Drives))
	}
	if cfg.Drives[0].DriveID != "rootfs" {
		t.Error("drive ID should be rootfs")
	}
	if !cfg.Drives[0].IsRootDevice {
		t.Error("should be root device")
	}
	if cfg.Drives[0].IsReadOnly {
		t.Error("should not be read-only")
	}
	if !strings.Contains(cfg.Drives[0].PathOnHost, "full-test/rootfs.ext4") {
		t.Errorf("drive path = %q", cfg.Drives[0].PathOnHost)
	}

	// Network
	if len(cfg.NetworkIfaces) != 1 {
		t.Fatalf("network ifaces len = %d, want 1", len(cfg.NetworkIfaces))
	}
	if cfg.NetworkIfaces[0].IfaceID != "net1" {
		t.Errorf("iface ID = %q, want net1", cfg.NetworkIfaces[0].IfaceID)
	}
	if cfg.NetworkIfaces[0].GuestMAC != alloc.GuestMAC {
		t.Errorf("MAC = %q, want %q", cfg.NetworkIfaces[0].GuestMAC, alloc.GuestMAC)
	}
	if cfg.NetworkIfaces[0].HostDevName != alloc.TAPDev {
		t.Errorf("host dev = %q, want %q", cfg.NetworkIfaces[0].HostDevName, alloc.TAPDev)
	}

	// Machine config
	if cfg.MachineConfig == nil {
		t.Fatal("MachineConfig should not be nil")
	}
	if cfg.MachineConfig.VcpuCount != 8 {
		t.Errorf("VcpuCount = %d, want 8", cfg.MachineConfig.VcpuCount)
	}
	if cfg.MachineConfig.MemSizeMiB != 4096 {
		t.Errorf("MemSizeMiB = %d, want 4096", cfg.MachineConfig.MemSizeMiB)
	}

	// Vsock
	if cfg.Vsock == nil {
		t.Fatal("Vsock should not be nil")
	}
	if cfg.Vsock.GuestCID != 3 {
		t.Errorf("GuestCID = %d, want 3", cfg.Vsock.GuestCID)
	}
	if !strings.Contains(cfg.Vsock.UDSPath, "full-test.vsock") {
		t.Errorf("UDSPath = %q", cfg.Vsock.UDSPath)
	}
}

// === Integration: StartScript contains all required elements ===

func TestStartScriptAllRequiredElements(t *testing.T) {
	alloc := state.AllocateNet(2)
	script := StartScript("integ-vm", alloc, 2, 1024)

	required := map[string]string{
		"set -e":           "should use strict mode",
		"integ-vm":         "should contain VM name",
		"tap2":             "should contain TAP device",
		"172.16.0.9":       "should contain TAP IP",
		"172.16.0.10":      "should contain guest IP",
		"firecracker":      "should launch firecracker",
		"--config-file":    "should use config file",
		"--api-sock":       "should set API socket",
		"--sparse=always":  "should use sparse copy",
		"base.ext4":        "should copy base rootfs",
		"rootfs.ext4":      "should target rootfs path",
		"ip tuntap add":    "should create TAP device",
		"ip addr add":      "should assign IP to TAP",
		"ip link set":      "should bring TAP up",
		"setsid":           "should detach process",
		"PID:$FC_PID":      "should output PID marker",
		"kill -0":          "should check process alive",
		"firecracker.log":  "should create log file",
		"/run/mvm":         "should use run directory",
		"/opt/mvm":         "should use opt directory",
		".vsock":           "should clean stale vsock",
	}

	for substr, reason := range required {
		if !strings.Contains(script, substr) {
			t.Errorf("StartScript missing %q: %s", substr, reason)
		}
	}
}

// === Integration: StartExistingScript vs StartScript ===

func TestStartExistingVsStartDifferences(t *testing.T) {
	alloc := state.AllocateNet(0)
	freshScript := StartScript("fresh", alloc, 0, 0)
	existingScript := StartExistingScript("existing", alloc, 0, 0)

	// Fresh should copy base.ext4, existing should NOT
	if !strings.Contains(freshScript, "base.ext4") {
		t.Error("StartScript should copy base.ext4")
	}
	if strings.Contains(existingScript, "base.ext4") {
		t.Error("StartExistingScript should NOT copy base.ext4")
	}

	// Existing should check rootfs exists
	if !strings.Contains(existingScript, "if [ ! -f") {
		t.Error("StartExistingScript should check rootfs exists")
	}

	// Both should have firecracker launch
	if !strings.Contains(freshScript, "firecracker") {
		t.Error("StartScript should launch firecracker")
	}
	if !strings.Contains(existingScript, "firecracker") {
		t.Error("StartExistingScript should launch firecracker")
	}

	// Both should report PID
	if !strings.Contains(freshScript, "PID:") {
		t.Error("StartScript should output PID")
	}
	if !strings.Contains(existingScript, "PID:") {
		t.Error("StartExistingScript should output PID")
	}
}

// === Integration: StartFromSnapshotScript differences ===

func TestSnapshotScriptVsFreshStartDifferences(t *testing.T) {
	alloc := state.AllocateNet(0)
	freshScript := StartScript("fresh", alloc, 0, 0)
	snapScript := StartFromSnapshotScript("snap", alloc)

	// Fresh uses --config-file, snapshot does NOT
	if !strings.Contains(freshScript, "--config-file") {
		t.Error("StartScript should use --config-file")
	}
	if strings.Contains(snapScript, "--config-file") {
		t.Error("StartFromSnapshotScript should NOT use --config-file")
	}

	// Snapshot uses curl for API, fresh does not
	if !strings.Contains(snapScript, "curl") {
		t.Error("StartFromSnapshotScript should use curl for snapshot/load")
	}

	// Snapshot has network_overrides
	if !strings.Contains(snapScript, "network_overrides") {
		t.Error("StartFromSnapshotScript must have network_overrides")
	}

	// Snapshot has resume_vm
	if !strings.Contains(snapScript, "resume_vm") {
		t.Error("StartFromSnapshotScript should resume VM")
	}

	// Snapshot waits for socket
	if !strings.Contains(snapScript, "test -S") {
		t.Error("StartFromSnapshotScript should wait for socket")
	}
}

// === Integration: Config matches script ===

func TestConfigMatchesScriptNetworking(t *testing.T) {
	alloc := state.AllocateNet(3)
	cfgStr, _ := GenerateConfig("match-test", alloc, 0, 0)
	script := StartScript("match-test", alloc, 0, 0)

	var cfg fcConfig
	json.Unmarshal([]byte(cfgStr), &cfg)

	// The TAP device in config must match the one set up in script
	tapDev := cfg.NetworkIfaces[0].HostDevName
	if !strings.Contains(script, tapDev) {
		t.Errorf("script should contain TAP device %q from config", tapDev)
	}

	// The guest MAC in config must match the allocation
	if cfg.NetworkIfaces[0].GuestMAC != alloc.GuestMAC {
		t.Errorf("config MAC %q != alloc MAC %q", cfg.NetworkIfaces[0].GuestMAC, alloc.GuestMAC)
	}

	// Boot args should contain the guest IP from the allocation
	if !strings.Contains(cfg.BootSource.BootArgs, alloc.GuestIP) {
		t.Error("boot args should contain guest IP from allocation")
	}
}

// === Integration: parsePID ===

func TestParsePIDFromOutput(t *testing.T) {
	tests := []struct {
		output string
		want   int
	}{
		{"PID:1234", 1234},
		{"Starting VM...\nNetwork ready\nPID:5678\n", 5678},
		{"no pid line", 0},
		{"PID:", 0},
		{"PID:abc", 0},
		{"", 0},
		{"PID:42\nPID:43", 42}, // first match
	}

	for _, tt := range tests {
		got := parsePID(tt.output)
		if got != tt.want {
			t.Errorf("parsePID(%q) = %d, want %d", tt.output, got, tt.want)
		}
	}
}

// === Integration: All 62 slots produce distinct configs ===

func TestAll62SlotsProduceDistinctConfigs(t *testing.T) {
	seenMAC := make(map[string]int)
	seenTAP := make(map[string]int)
	seenGuestIP := make(map[string]int)

	for i := 0; i < 62; i++ {
		alloc := state.AllocateNet(i)
		cfgStr, err := GenerateConfig("vm", alloc, 0, 0)
		if err != nil {
			t.Fatalf("index %d: %v", i, err)
		}

		var cfg fcConfig
		json.Unmarshal([]byte(cfgStr), &cfg)

		mac := cfg.NetworkIfaces[0].GuestMAC
		tap := cfg.NetworkIfaces[0].HostDevName
		guestIP := alloc.GuestIP

		if prev, ok := seenMAC[mac]; ok {
			t.Errorf("MAC collision: index %d and %d both got %s", prev, i, mac)
		}
		seenMAC[mac] = i

		if prev, ok := seenTAP[tap]; ok {
			t.Errorf("TAP collision: index %d and %d both got %s", prev, i, tap)
		}
		seenTAP[tap] = i

		if prev, ok := seenGuestIP[guestIP]; ok {
			t.Errorf("GuestIP collision: index %d and %d both got %s", prev, i, guestIP)
		}
		seenGuestIP[guestIP] = i
	}
}

// === Integration: Default resources match constants ===

func TestDefaultResourcesMatchConstants(t *testing.T) {
	alloc := state.AllocateNet(0)
	cfgStr, _ := GenerateConfig("defaults", alloc, 0, 0)

	var cfg fcConfig
	json.Unmarshal([]byte(cfgStr), &cfg)

	if cfg.MachineConfig.VcpuCount != GuestVcpuCount {
		t.Errorf("default VcpuCount = %d, want %d", cfg.MachineConfig.VcpuCount, GuestVcpuCount)
	}
	if cfg.MachineConfig.MemSizeMiB != GuestMemSizeMiB {
		t.Errorf("default MemSizeMiB = %d, want %d", cfg.MachineConfig.MemSizeMiB, GuestMemSizeMiB)
	}
}

// === Integration: Custom resources override defaults ===

func TestCustomResourcesOverrideDefaults(t *testing.T) {
	tests := []struct {
		cpus   int
		memMB  int
		wantC  int
		wantM  int
	}{
		{0, 0, GuestVcpuCount, GuestMemSizeMiB}, // defaults
		{1, 512, 1, 512},                          // custom
		{16, 8192, 16, 8192},                      // large
		{0, 1024, GuestVcpuCount, 1024},           // only memory custom
		{8, 0, 8, GuestMemSizeMiB},               // only cpus custom
	}

	for _, tt := range tests {
		alloc := state.AllocateNet(0)
		cfgStr, _ := GenerateConfig("res-test", alloc, tt.cpus, tt.memMB)

		var cfg fcConfig
		json.Unmarshal([]byte(cfgStr), &cfg)

		if cfg.MachineConfig.VcpuCount != tt.wantC {
			t.Errorf("cpus=%d: VcpuCount = %d, want %d", tt.cpus, cfg.MachineConfig.VcpuCount, tt.wantC)
		}
		if cfg.MachineConfig.MemSizeMiB != tt.wantM {
			t.Errorf("memMB=%d: MemSizeMiB = %d, want %d", tt.memMB, cfg.MachineConfig.MemSizeMiB, tt.wantM)
		}
	}
}

// === Integration: BootArgs has correct kernel IP format ===

func TestBootArgsKernelIPFormat(t *testing.T) {
	tests := []struct {
		guestIP   string
		gatewayIP string
	}{
		{"172.16.0.2", "172.16.0.1"},
		{"172.16.0.6", "172.16.0.5"},
		{"10.0.0.2", "10.0.0.1"},
	}

	for _, tt := range tests {
		args := BootArgs(tt.guestIP, tt.gatewayIP)

		// Must contain ip= kernel parameter
		expected := "ip=" + tt.guestIP + "::" + tt.gatewayIP
		if !strings.Contains(args, expected) {
			t.Errorf("BootArgs(%s, %s) missing %q in: %s", tt.guestIP, tt.gatewayIP, expected, args)
		}

		// Must contain netmask
		if !strings.Contains(args, "255.255.255.252") {
			t.Error("BootArgs should contain /30 netmask")
		}

		// Must contain eth0
		if !strings.Contains(args, "eth0") {
			t.Error("BootArgs should reference eth0")
		}
	}
}

// === Security: shellQuoteForSSH roundtrip ===

func TestShellQuoteForSSHRoundTrip(t *testing.T) {
	tests := []string{
		"hello",
		"hello world",
		"it's",
		"",
		"a'b'c",
		"$(rm -rf /)",
		"`cat /etc/shadow`",
		"; echo pwned",
		"|| true",
		"&& malicious",
		"multi\nline",
		"tab\there",
	}

	for _, input := range tests {
		quoted := shellQuoteForSSH(input)
		// Quoted string should start and end with single quote
		if len(quoted) < 2 || quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
			t.Errorf("shellQuoteForSSH(%q) = %q, not properly quoted", input, quoted)
		}
	}
}

// === Constants sanity checks ===

func TestDirectoryConstants(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"CacheDir", CacheDir()},
		{"VMsDir", VMsDir()},
		{"KeyDir", KeyDir()},
		{"RunDir", RunDir()},
		{"SnapshotDir", SnapshotDir()},
	}

	for _, tt := range tests {
		if tt.value == "" {
			t.Errorf("%s should not be empty", tt.name)
		}
		if !strings.HasPrefix(tt.value, "/") {
			t.Errorf("%s = %q, should be absolute path", tt.name, tt.value)
		}
	}
}

func TestDefaultResourceConstants(t *testing.T) {
	if GuestVcpuCount <= 0 {
		t.Errorf("GuestVcpuCount = %d, should be > 0", GuestVcpuCount)
	}
	if GuestMemSizeMiB <= 0 {
		t.Errorf("GuestMemSizeMiB = %d, should be > 0", GuestMemSizeMiB)
	}
	// Reasonable ranges
	if GuestVcpuCount > 32 {
		t.Errorf("GuestVcpuCount = %d, seems too high", GuestVcpuCount)
	}
	if GuestMemSizeMiB > 65536 {
		t.Errorf("GuestMemSizeMiB = %d, seems too high", GuestMemSizeMiB)
	}
}
