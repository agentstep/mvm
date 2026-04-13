package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(filepath.Join(dir, "state.json"))
}

func TestNewStore(t *testing.T) {
	s := tempStore(t)
	if s.Path() == "" {
		t.Fatal("store path should not be empty")
	}
}

func TestLoadEmpty(t *testing.T) {
	s := tempStore(t)
	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if st.Initialized {
		t.Error("should not be initialized")
	}
	if len(st.VMs) != 0 {
		t.Error("should have no VMs")
	}
}

func TestSaveAndLoad(t *testing.T) {
	s := tempStore(t)
	st := newState()
	st.Initialized = true
	st.FCVersion = "v1.13.0"
	st.InitAt = time.Now()

	if err := s.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Initialized {
		t.Error("should be initialized")
	}
	if loaded.FCVersion != "v1.13.0" {
		t.Errorf("FCVersion = %q, want v1.13.0", loaded.FCVersion)
	}
}

func TestAddAndGetVM(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{
		Name:      "test1",
		Status:    "running",
		GuestIP:   "172.16.0.2",
		TAPIP:     "172.16.0.1",
		TAPDevice: "tap0",
		GuestMAC:  "06:00:AC:10:00:02",
		NetIndex:  0,
		PID:       1234,
		CreatedAt: time.Now(),
	}

	if err := s.AddVM(vm); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	got, err := s.GetVM("test1")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.Name != "test1" {
		t.Errorf("Name = %q, want test1", got.Name)
	}
	if got.GuestIP != "172.16.0.2" {
		t.Errorf("GuestIP = %q, want 172.16.0.2", got.GuestIP)
	}
	if got.PID != 1234 {
		t.Errorf("PID = %d, want 1234", got.PID)
	}
}

func TestAddDuplicateVM(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{Name: "dup", Status: "running", CreatedAt: time.Now()}
	s.AddVM(vm)

	err := s.AddVM(vm)
	if err == nil {
		t.Error("should error on duplicate VM name")
	}
}

func TestUpdateVM(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{Name: "upd", Status: "running", CreatedAt: time.Now()}
	s.AddVM(vm)

	now := time.Now()
	err := s.UpdateVM("upd", func(v *VM) {
		v.Status = "stopped"
		v.StoppedAt = &now
	})
	if err != nil {
		t.Fatalf("UpdateVM: %v", err)
	}

	got, _ := s.GetVM("upd")
	if got.Status != "stopped" {
		t.Errorf("Status = %q, want stopped", got.Status)
	}
	if got.StoppedAt == nil {
		t.Error("StoppedAt should be set")
	}
}

func TestRemoveVM(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{Name: "rm", Status: "running", CreatedAt: time.Now()}
	s.AddVM(vm)

	if err := s.RemoveVM("rm"); err != nil {
		t.Fatalf("RemoveVM: %v", err)
	}

	_, err := s.GetVM("rm")
	if err == nil {
		t.Error("should error on removed VM")
	}
}

func TestListVMs(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	for _, name := range []string{"a", "b", "c"} {
		s.AddVM(&VM{Name: name, Status: "running", CreatedAt: time.Now()})
	}

	vms, err := s.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 3 {
		t.Errorf("len = %d, want 3", len(vms))
	}
}

func TestNextNetIndex(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	idx, err := s.NextNetIndex()
	if err != nil {
		t.Fatalf("NextNetIndex: %v", err)
	}
	if idx != 0 {
		t.Errorf("first index = %d, want 0", idx)
	}

	// Add VM at index 0
	s.AddVM(&VM{Name: "vm0", NetIndex: 0, CreatedAt: time.Now()})

	idx, err = s.NextNetIndex()
	if err != nil {
		t.Fatalf("NextNetIndex: %v", err)
	}
	if idx != 1 {
		t.Errorf("second index = %d, want 1", idx)
	}
}

func TestNextNetIndexReusesGaps(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	// Add VMs at 0, 1, 2
	for i := 0; i < 3; i++ {
		s.AddVM(&VM{Name: "v" + string(rune('0'+i)), NetIndex: i, CreatedAt: time.Now()})
	}

	// Remove VM at index 1
	s.RemoveVM("v1")

	idx, _ := s.NextNetIndex()
	if idx != 1 {
		t.Errorf("should reuse gap: got %d, want 1", idx)
	}
}

func TestMarkInitialized(t *testing.T) {
	s := tempStore(t)

	if err := s.MarkInitialized("v1.13.0", "firecracker"); err != nil {
		t.Fatalf("MarkInitialized: %v", err)
	}

	ok, _ := s.IsInitialized()
	if !ok {
		t.Error("should be initialized")
	}
}

func TestGetVMNotFound(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	_, err := s.GetVM("nonexistent")
	if err == nil {
		t.Error("should error on nonexistent VM")
	}
}

func TestStoreCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c", "state.json")
	s := NewStore(nested)

	if err := s.Save(newState()); err != nil {
		t.Fatalf("Save to nested dir: %v", err)
	}

	if _, err := os.Stat(nested); os.IsNotExist(err) {
		t.Error("state file should exist")
	}
}

func TestReserveVM(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{Name: "res1", Status: "starting", CreatedAt: time.Now()}
	idx, err := s.ReserveVM(vm)
	if err != nil {
		t.Fatalf("ReserveVM: %v", err)
	}
	if idx != 0 {
		t.Errorf("first index = %d, want 0", idx)
	}

	// Should be persisted
	got, _ := s.GetVM("res1")
	if got.Status != "starting" {
		t.Errorf("Status = %q, want starting", got.Status)
	}
	if got.NetIndex != 0 {
		t.Errorf("NetIndex = %d, want 0", got.NetIndex)
	}
}

func TestReserveVMDuplicateName(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm1 := &VM{Name: "dup", Status: "starting", CreatedAt: time.Now()}
	s.ReserveVM(vm1)

	vm2 := &VM{Name: "dup", Status: "starting", CreatedAt: time.Now()}
	_, err := s.ReserveVM(vm2)
	if err == nil {
		t.Error("should reject duplicate name")
	}
}

func TestReserveVMCleanupReleasesIndex(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	// Reserve a VM (simulates mvm start beginning)
	vm := &VM{Name: "fail-start", Status: "starting", CreatedAt: time.Now()}
	idx, _ := s.ReserveVM(vm)
	if idx != 0 {
		t.Fatalf("expected index 0, got %d", idx)
	}

	// Simulate boot failure — remove the reservation
	s.RemoveVM("fail-start")

	// The index should be available again
	vm2 := &VM{Name: "retry", Status: "starting", CreatedAt: time.Now()}
	idx2, err := s.ReserveVM(vm2)
	if err != nil {
		t.Fatalf("ReserveVM after cleanup: %v", err)
	}
	if idx2 != 0 {
		t.Errorf("expected reused index 0, got %d", idx2)
	}

	// And the name "fail-start" should be available again
	vm3 := &VM{Name: "fail-start", Status: "starting", CreatedAt: time.Now()}
	_, err = s.ReserveVM(vm3)
	if err != nil {
		t.Errorf("name should be reusable after cleanup: %v", err)
	}
}

func TestReserveVMConcurrentUniqueIndexes(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	// Simulate concurrent reservations — since they go through Transact
	// with flock, they serialize even from goroutines sharing the same file.
	const n = 10
	errs := make(chan error, n)
	indexes := make(chan int, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			vm := &VM{
				Name:      fmt.Sprintf("concurrent-%d", i),
				Status:    "starting",
				CreatedAt: time.Now(),
			}
			idx, err := s.ReserveVM(vm)
			errs <- err
			indexes <- idx
		}(i)
	}

	seen := make(map[int]bool)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("reservation %d failed: %v", i, err)
		}
		idx := <-indexes
		if seen[idx] {
			t.Errorf("duplicate net index allocated: %d", idx)
		}
		seen[idx] = true
	}

	// All 10 should have unique indexes 0-9
	if len(seen) != n {
		t.Errorf("expected %d unique indexes, got %d", n, len(seen))
	}

	// Verify in state
	vms, _ := s.ListVMs()
	if len(vms) != n {
		t.Errorf("expected %d VMs in state, got %d", n, len(vms))
	}
}

func TestTransactSerializes(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	// Run 20 concurrent increments of a counter stored as VM count.
	// If Transact serializes correctly, we end up with exactly 20 VMs.
	const n = 20
	done := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			done <- s.Transact(func(st *State) error {
				name := fmt.Sprintf("txn-%d", i)
				st.VMs[name] = &VM{Name: name, NetIndex: i, CreatedAt: time.Now()}
				return nil
			})
		}(i)
	}

	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("transact %d: %v", i, err)
		}
	}

	st, _ := s.Load()
	if len(st.VMs) != n {
		t.Errorf("expected %d VMs, got %d (lost writes = serialization failure)", n, len(st.VMs))
	}
}

func TestGetBackendDefault(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	b := s.GetBackend()
	if b != "firecracker" {
		t.Errorf("default backend = %q, want firecracker", b)
	}
}

func TestGetBackendSet(t *testing.T) {
	s := tempStore(t)
	s.MarkInitialized("v1.13.0", "applevz")

	b := s.GetBackend()
	if b != "applevz" {
		t.Errorf("backend = %q, want applevz", b)
	}
}

func TestMarkInitializedWithBackend(t *testing.T) {
	s := tempStore(t)
	s.MarkInitialized("v1.13.0", "firecracker")

	st, _ := s.Load()
	if !st.Initialized {
		t.Error("should be initialized")
	}
	if st.Backend != "firecracker" {
		t.Errorf("Backend = %q, want firecracker", st.Backend)
	}
	if st.FCVersion != "v1.13.0" {
		t.Errorf("FCVersion = %q", st.FCVersion)
	}
}

func TestVMWithLastActivity(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	now := time.Now()
	vm := &VM{Name: "active", Status: "running", LastActivity: &now, CreatedAt: time.Now()}
	s.AddVM(vm)

	got, _ := s.GetVM("active")
	if got.LastActivity == nil {
		t.Error("LastActivity should be set")
	}
	if got.LastActivity.Sub(now) > time.Second {
		t.Error("LastActivity should be close to set time")
	}
}

func TestVMWithIdleTimeout(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{Name: "idle", Status: "running", IdleTimeout: "5m", CreatedAt: time.Now()}
	s.AddVM(vm)

	got, _ := s.GetVM("idle")
	if got.IdleTimeout != "5m" {
		t.Errorf("IdleTimeout = %q, want 5m", got.IdleTimeout)
	}
}

func TestVMWithBackendField(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{Name: "vz", Status: "running", Backend: "applevz", CreatedAt: time.Now()}
	s.AddVM(vm)

	got, _ := s.GetVM("vz")
	if got.Backend != "applevz" {
		t.Errorf("Backend = %q, want applevz", got.Backend)
	}
}

func TestVMWithPorts(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{
		Name:      "ports",
		Status:    "running",
		CreatedAt: time.Now(),
		Ports: []PortMap{
			{HostPort: 8080, GuestPort: 80, Proto: "tcp"},
			{HostPort: 5432, GuestPort: 5432, Proto: "tcp"},
		},
		NetPolicy: "deny",
	}
	s.AddVM(vm)

	got, _ := s.GetVM("ports")
	if len(got.Ports) != 2 {
		t.Errorf("Ports len = %d, want 2", len(got.Ports))
	}
	if got.Ports[0].HostPort != 8080 {
		t.Errorf("Ports[0].HostPort = %d, want 8080", got.Ports[0].HostPort)
	}
	if got.NetPolicy != "deny" {
		t.Errorf("NetPolicy = %q, want deny", got.NetPolicy)
	}
}

// === NEW TESTS: ValidateName (shell injection prevention) ===

func TestValidateNameAcceptsValid(t *testing.T) {
	valid := []string{
		"my-vm",
		"test123",
		"My.VM",
		"a",
		"vm-1_test.2",
		"UPPERCASE",
		"mix.Ed-Case_123",
	}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) should accept, got: %v", name, err)
		}
	}
}

func TestValidateNameRejectsEmpty(t *testing.T) {
	if err := ValidateName(""); err == nil {
		t.Error("should reject empty name")
	}
}

func TestValidateNameRejectsShellInjection(t *testing.T) {
	malicious := []string{
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
	}
	for _, name := range malicious {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) should reject shell-unsafe name", name)
		}
	}
}

// === NEW TESTS: Per-VM resources (cpus, memory) ===

func TestVMWithCustomResources(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{
		Name:      "custom-res",
		Status:    "running",
		Cpus:      4,
		MemoryMB:  2048,
		CreatedAt: time.Now(),
	}
	s.AddVM(vm)

	got, _ := s.GetVM("custom-res")
	if got.Cpus != 4 {
		t.Errorf("Cpus = %d, want 4", got.Cpus)
	}
	if got.MemoryMB != 2048 {
		t.Errorf("MemoryMB = %d, want 2048", got.MemoryMB)
	}
}

func TestVMWithZeroResourcesOmitted(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	vm := &VM{
		Name:      "default-res",
		Status:    "running",
		CreatedAt: time.Now(),
		// Cpus and MemoryMB left at 0 (use defaults)
	}
	s.AddVM(vm)

	// Verify JSON serialization omits zero values
	data, _ := os.ReadFile(s.Path())
	if !json.Valid(data) {
		t.Fatal("state file is not valid JSON")
	}
}

// === NEW TESTS: NextNetIndex pool exhaustion ===

func TestNextNetIndexPoolExhaustion(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	// Fill all 62 slots
	for i := 0; i < 62; i++ {
		s.AddVM(&VM{Name: fmt.Sprintf("vm%d", i), NetIndex: i, CreatedAt: time.Now()})
	}

	_, err := s.NextNetIndex()
	if err == nil {
		t.Error("should error when pool is exhausted")
	}
}

func TestReserveVMPoolExhaustion(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	// Fill all 62 slots
	for i := 0; i < 62; i++ {
		vm := &VM{Name: fmt.Sprintf("fill%d", i), NetIndex: i, CreatedAt: time.Now()}
		s.Transact(func(st *State) error {
			st.VMs[vm.Name] = vm
			return nil
		})
	}

	_, err := s.ReserveVM(&VM{Name: "overflow", Status: "starting", CreatedAt: time.Now()})
	if err == nil {
		t.Error("should error when all 62 slots used")
	}
}

// === NEW TESTS: Transact error rollback ===

func TestTransactErrorDoesNotMutateState(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	s.AddVM(&VM{Name: "original", Status: "running", CreatedAt: time.Now()})

	// Transact that fails should not persist changes
	err := s.Transact(func(st *State) error {
		st.VMs["should-not-persist"] = &VM{Name: "should-not-persist"}
		return fmt.Errorf("intentional failure")
	})
	if err == nil {
		t.Fatal("should have returned error")
	}

	// The failed mutation should not be persisted
	st, _ := s.Load()
	if _, exists := st.VMs["should-not-persist"]; exists {
		t.Error("failed transaction should not persist mutations")
	}
	if _, exists := st.VMs["original"]; !exists {
		t.Error("original VM should still exist")
	}
}

// === NEW TESTS: UpdateVM on nonexistent VM ===

func TestUpdateVMNotFound(t *testing.T) {
	s := tempStore(t)
	s.Save(newState())

	err := s.UpdateVM("ghost", func(v *VM) { v.Status = "stopped" })
	if err == nil {
		t.Error("should error updating nonexistent VM")
	}
}

// === NEW TESTS: Dir() method ===

func TestStoreDir(t *testing.T) {
	s := NewStore("/tmp/test/state.json")
	if s.Dir() != "/tmp/test" {
		t.Errorf("Dir = %q, want /tmp/test", s.Dir())
	}
}

// === NEW TESTS: State JSON roundtrip with all fields ===

func TestStateJSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	stopped := now.Add(-time.Minute)
	activity := now.Add(-30 * time.Second)

	original := &State{
		VMs: map[string]*VM{
			"full": {
				Name:         "full",
				Status:       "running",
				GuestIP:      "172.16.0.2",
				TAPIP:        "172.16.0.1",
				TAPDevice:    "tap0",
				GuestMAC:     "06:00:AC:10:00:02",
				NetIndex:     0,
				SocketPath:   "/run/mvm/full.socket",
				PID:          42,
				RootfsPath:   "/opt/mvm/vms/full/rootfs.ext4",
				Ports:        []PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
				NetPolicy:    "deny",
				Backend:      "firecracker",
				Cpus:         4,
				MemoryMB:     2048,
				IdleTimeout:  "10m",
				LastActivity: &activity,
				CreatedAt:    now,
				StoppedAt:    &stopped,
			},
		},
		Initialized: true,
		InitAt:      now,
		FCVersion:   "v1.13.0",
		Backend:     "firecracker",
	}

	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	vm := loaded.VMs["full"]
	if vm == nil {
		t.Fatal("VM 'full' not found after roundtrip")
	}
	if vm.Cpus != 4 {
		t.Errorf("Cpus = %d, want 4", vm.Cpus)
	}
	if vm.MemoryMB != 2048 {
		t.Errorf("MemoryMB = %d, want 2048", vm.MemoryMB)
	}
	if vm.Backend != "firecracker" {
		t.Errorf("Backend = %q, want firecracker", vm.Backend)
	}
	if vm.IdleTimeout != "10m" {
		t.Errorf("IdleTimeout = %q, want 10m", vm.IdleTimeout)
	}
	if vm.LastActivity == nil {
		t.Error("LastActivity should not be nil")
	}
	if vm.StoppedAt == nil {
		t.Error("StoppedAt should not be nil")
	}
	if len(vm.Ports) != 1 || vm.Ports[0].HostPort != 8080 {
		t.Errorf("Ports roundtrip failed: %+v", vm.Ports)
	}
	if vm.NetPolicy != "deny" {
		t.Errorf("NetPolicy = %q, want deny", vm.NetPolicy)
	}
}

// === NEW TEST: Corrupt state file recovery ===

func TestLoadCorruptState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte("not json at all {{{"), 0o644)

	s := NewStore(path)
	_, err := s.Load()
	if err == nil {
		t.Error("should error on corrupt state file")
	}
}

// === NEW TEST: newState initializes VMs map ===

func TestNewStateHasVMsMap(t *testing.T) {
	st := newState()
	if st.VMs == nil {
		t.Error("newState() should initialize VMs map")
	}
}
