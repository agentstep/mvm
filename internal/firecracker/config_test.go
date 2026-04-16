package firecracker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

func TestBootArgs(t *testing.T) {
	args := BootArgs("172.16.0.2", "172.16.0.1")

	required := []string{
		"console=ttyS0",
		"reboot=k",
		"panic=1",
		"quiet",
		"random.trust_cpu=on",
		"rootfstype=ext4",
		"ip=172.16.0.2::172.16.0.1",
		"keep_bootcon",
	}
	for _, r := range required {
		if !strings.Contains(args, r) {
			t.Errorf("BootArgs missing %q", r)
		}
	}
}

func TestSocketPath(t *testing.T) {
	p := SocketPath("myvm")
	if p != "/run/mvm/myvm.socket" {
		t.Errorf("SocketPath = %q, want /run/mvm/myvm.socket", p)
	}
}

func TestVMDir(t *testing.T) {
	d := VMDir("test")
	if d != "/opt/mvm/vms/test" {
		t.Errorf("VMDir = %q, want /opt/mvm/vms/test", d)
	}
}

func TestGenerateConfig(t *testing.T) {
	alloc := state.AllocateNet(0)
	cfgStr, err := GenerateConfig("test", alloc, 0, 0)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	// Should be valid JSON
	var cfg fcConfig
	if err := json.Unmarshal([]byte(cfgStr), &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Check boot source
	if !strings.Contains(cfg.BootSource.KernelImagePath, "vmlinux") {
		t.Error("kernel path should contain vmlinux")
	}
	if !strings.Contains(cfg.BootSource.BootArgs, "172.16.0.2") {
		t.Error("boot args should contain guest IP")
	}

	// Check drives
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
	if !strings.Contains(cfg.Drives[0].PathOnHost, "test/rootfs.ext4") {
		t.Errorf("drive path = %q, should contain test/rootfs.ext4", cfg.Drives[0].PathOnHost)
	}

	// Check network
	if len(cfg.NetworkIfaces) != 1 {
		t.Fatalf("network ifaces len = %d, want 1", len(cfg.NetworkIfaces))
	}
	if cfg.NetworkIfaces[0].GuestMAC != "06:00:AC:10:00:02" {
		t.Errorf("MAC = %q, want 06:00:AC:10:00:02", cfg.NetworkIfaces[0].GuestMAC)
	}
	if cfg.NetworkIfaces[0].HostDevName != "tap0" {
		t.Errorf("host dev = %q, want tap0", cfg.NetworkIfaces[0].HostDevName)
	}
}

func TestGenerateConfigDifferentIndex(t *testing.T) {
	alloc := state.AllocateNet(5)
	cfgStr, _ := GenerateConfig("vm5", alloc, 0, 0)

	var cfg fcConfig
	json.Unmarshal([]byte(cfgStr), &cfg)

	if cfg.NetworkIfaces[0].HostDevName != "tap5" {
		t.Errorf("host dev = %q, want tap5", cfg.NetworkIfaces[0].HostDevName)
	}
	if !strings.Contains(cfg.BootSource.BootArgs, "172.16.0.22") {
		t.Errorf("boot args should contain guest IP 172.16.0.22, got: %s", cfg.BootSource.BootArgs)
	}
}

func TestStartScript(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("test", alloc, 0, 0)

	checks := []string{
		"tap0",
		"172.16.0.1",
		"172.16.0.2",
		"firecracker",
		"--config-file",
				"setsid",
		"PID:",
	}
	for _, c := range checks {
		if !strings.Contains(script, c) {
			t.Errorf("StartScript missing %q", c)
		}
	}

	// Should NOT contain curl (we use --config-file now)
	if strings.Contains(script, "curl") {
		t.Error("StartScript should not use curl (use --config-file)")
	}
}

func TestStartFromSnapshotScript(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartFromSnapshotScript("snap-test", alloc)

	checks := []string{
		"snapshot",
		"snapshot_file",
		"mem_file",
		"resume_vm",
		"firecracker",
		"--api-sock",
				"PID:",
	}
	for _, c := range checks {
		if !strings.Contains(script, c) {
			t.Errorf("StartFromSnapshotScript missing %q", c)
		}
	}
}

func TestPathFunctions(t *testing.T) {
	if CacheDir() == "" {
		t.Error("CacheDir() should not be empty")
	}
	if VMsDir() == "" {
		t.Error("VMsDir() should not be empty")
	}
	if KeyDir() == "" {
		t.Error("KeyDir() should not be empty")
	}
	if RunDir() == "" {
		t.Error("RunDir() should not be empty")
	}
	if SnapshotDir() == "" {
		t.Error("SnapshotDir() should not be empty")
	}
}

// === NEW TESTS: Custom CPU/memory in GenerateConfig ===

func TestGenerateConfigCustomResources(t *testing.T) {
	alloc := state.AllocateNet(0)
	cfgStr, err := GenerateConfig("custom", alloc, 8, 4096)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	var cfg fcConfig
	json.Unmarshal([]byte(cfgStr), &cfg)

	if cfg.MachineConfig == nil {
		t.Fatal("MachineConfig should not be nil")
	}
	if cfg.MachineConfig.VcpuCount != 8 {
		t.Errorf("VcpuCount = %d, want 8", cfg.MachineConfig.VcpuCount)
	}
	if cfg.MachineConfig.MemSizeMiB != 4096 {
		t.Errorf("MemSizeMiB = %d, want 4096", cfg.MachineConfig.MemSizeMiB)
	}
}

func TestGenerateConfigDefaultResources(t *testing.T) {
	alloc := state.AllocateNet(0)
	cfgStr, _ := GenerateConfig("default", alloc, 0, 0)

	var cfg fcConfig
	json.Unmarshal([]byte(cfgStr), &cfg)

	if cfg.MachineConfig.VcpuCount != GuestVcpuCount {
		t.Errorf("VcpuCount = %d, want default %d", cfg.MachineConfig.VcpuCount, GuestVcpuCount)
	}
	if cfg.MachineConfig.MemSizeMiB != GuestMemSizeMiB {
		t.Errorf("MemSizeMiB = %d, want default %d", cfg.MachineConfig.MemSizeMiB, GuestMemSizeMiB)
	}
}

// === NEW TEST: Vsock config ===

func TestGenerateConfigHasVsock(t *testing.T) {
	alloc := state.AllocateNet(0)
	cfgStr, _ := GenerateConfig("vsock-test", alloc, 0, 0)

	var cfg fcConfig
	json.Unmarshal([]byte(cfgStr), &cfg)

	if cfg.Vsock == nil {
		t.Fatal("Vsock should be present in config")
	}
	if cfg.Vsock.GuestCID != 3 {
		t.Errorf("GuestCID = %d, want 3 (0-2 reserved)", cfg.Vsock.GuestCID)
	}
	if !strings.Contains(cfg.Vsock.UDSPath, "vsock-test.vsock") {
		t.Errorf("UDSPath = %q, should contain VM name", cfg.Vsock.UDSPath)
	}
}

// === NEW TEST: StartExistingScript doesn't copy base image ===

func TestStartExistingScriptNoCopy(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartExistingScript("existing", alloc, 0, 0)

	// Should verify rootfs exists instead of copying base.ext4
	if strings.Contains(script, "base.ext4") {
		t.Error("StartExistingScript should NOT copy base.ext4 (preserves user's rootfs)")
	}
	if !strings.Contains(script, "rootfs.ext4") {
		t.Error("should reference rootfs.ext4")
	}
	if !strings.Contains(script, "Restarting") {
		t.Error("should indicate restart, not fresh start")
	}
}

func TestStartExistingScriptChecksRootfs(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartExistingScript("existing", alloc, 2, 512)

	// Must check that rootfs exists before trying to boot
	if !strings.Contains(script, "if [ ! -f") {
		t.Error("should check rootfs exists before boot")
	}
}

// === NEW TEST: StartScript copies rootfs ===

func TestStartScriptCopiesRootfs(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("fresh", alloc, 0, 0)

	// Fresh start should copy from base image
	if !strings.Contains(script, "base.ext4") {
		t.Error("StartScript should copy base.ext4 for fresh start")
	}
	if !strings.Contains(script, "--sparse=always") {
		t.Error("should use sparse copy for speed")
	}
}

// === NEW TEST: StartFromSnapshotScript has network_overrides ===

func TestStartFromSnapshotScriptNetworkOverrides(t *testing.T) {
	alloc := state.AllocateNet(3)
	script := StartFromSnapshotScript("snap-net", alloc)

	// Must use network_overrides to remap TAP device during snapshot restore.
	// This was a critical fix: FC v1.13 rejects /network-interfaces after restore.
	if !strings.Contains(script, "network_overrides") {
		t.Error("snapshot restore must use network_overrides for TAP remapping")
	}
	if !strings.Contains(script, "tap3") {
		t.Error("should use the correct TAP device for the allocation")
	}
	if !strings.Contains(script, "net1") {
		t.Error("should reference iface_id net1")
	}
}

// === NEW TEST: Config JSON is pretty-printed ===

func TestGenerateConfigPrettyPrinted(t *testing.T) {
	alloc := state.AllocateNet(0)
	cfgStr, _ := GenerateConfig("pretty", alloc, 0, 0)

	if !strings.Contains(cfgStr, "\n") {
		t.Error("config should be pretty-printed (indented)")
	}
}

// === NEW TEST: StartScript uses MMIO (no --enable-pci) ===

func TestStartScriptMMIO(t *testing.T) {
	alloc := state.AllocateNet(0)
	script := StartScript("mmio-test", alloc, 0, 0)

	if strings.Contains(script, "--enable-pci") {
		t.Error("should NOT use --enable-pci (MMIO transport for vsock compatibility)")
	}
}

// === NEW TEST: BootArgs kernel networking format ===

func TestBootArgsNetworkFormat(t *testing.T) {
	args := BootArgs("10.0.0.2", "10.0.0.1")

	// Format: ip=<guest>::<gateway>:<netmask>::<device>:<mode>
	if !strings.Contains(args, "ip=10.0.0.2::10.0.0.1:255.255.255.252::eth0:off") {
		t.Errorf("BootArgs kernel ip= format incorrect: %s", args)
	}
}
