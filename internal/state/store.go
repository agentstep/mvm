package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// VM represents a single microVM's state.
type VM struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"` // "running", "paused", "stopped"
	GuestIP     string     `json:"guest_ip"`
	TAPIP       string     `json:"tap_ip"`
	TAPDevice   string     `json:"tap_device"`
	GuestMAC    string     `json:"guest_mac"`
	NetIndex    int        `json:"net_index"`
	SocketPath  string     `json:"socket_path"`
	PID         int        `json:"pid"`
	UFFDPid     int        `json:"uffd_pid,omitempty"` // mvm-uffd sidecar PID (0 = File backend)
	RootfsPath  string     `json:"rootfs_path"`
	Ports       []PortMap  `json:"ports,omitempty"`
	NetPolicy   string     `json:"net_policy,omitempty"` // "open", "deny", "allow:<domains>"
	Backend      string         `json:"backend,omitempty"`      // "firecracker" or "applevz"
	Cpus         int            `json:"cpus,omitempty"`         // vCPU count (0 = default)
	MemoryMB     int            `json:"memory_mb,omitempty"`    // RAM in MiB (0 = default)
	IdleTimeout  string         `json:"idle_timeout,omitempty"` // e.g. "5m"
	LastActivity *time.Time     `json:"last_activity,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	StoppedAt    *time.Time     `json:"stopped_at,omitempty"`
}

// PortMap represents a host:guest port forwarding rule.
type PortMap struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"` // "tcp" or "udp"
}

// State holds all mvm state.
type State struct {
	VMs         map[string]*VM `json:"vms"`
	Initialized bool           `json:"initialized"`
	InitAt      time.Time      `json:"init_at"`
	FCVersion   string         `json:"fc_version"`
	Backend     string         `json:"backend,omitempty"` // "firecracker" or "applevz"
}

func newState() *State {
	return &State{
		VMs: make(map[string]*VM),
	}
}

// Store manages persistent state in a JSON file with file locking.
type Store struct {
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the state file path.
func (s *Store) Path() string {
	return s.path
}

// Dir returns the directory containing the state file.
func (s *Store) Dir() string {
	return filepath.Dir(s.path)
}

// Transact performs an atomic read-modify-write on the state file.
// The file is locked for the entire duration, preventing races between
// concurrent mvm processes.
func (s *Store) Transact(fn func(*State) error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open state file: %w", err)
	}
	defer f.Close()

	// Hold exclusive lock for entire read-modify-write
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock state file: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// Read current state
	data, err := os.ReadFile(s.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read state: %w", err)
	}

	st := newState()
	if len(data) > 0 {
		if err := json.Unmarshal(data, st); err != nil {
			return fmt.Errorf("parse state: %w", err)
		}
		if st.VMs == nil {
			st.VMs = make(map[string]*VM)
		}
	}

	// Apply mutation
	if err := fn(st); err != nil {
		return err
	}

	// Write back
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate state file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek state file: %w", err)
	}
	if _, err := f.Write(out); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	// Sync before releasing lock to ensure other processes see the write
	return f.Sync()
}

// Load reads state from disk (unlocked read — use for display only, not mutations).
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return newState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if st.VMs == nil {
		st.VMs = make(map[string]*VM)
	}
	return &st, nil
}

// Save writes state to disk with file locking.
func (s *Store) Save(st *State) error {
	return s.Transact(func(current *State) error {
		*current = *st
		return nil
	})
}

// GetVM returns a VM by name.
func (s *Store) GetVM(name string) (*VM, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	vm, ok := st.VMs[name]
	if !ok {
		return nil, fmt.Errorf("no microVM named %q. Run: mvm list", name)
	}
	return vm, nil
}

// AddVM atomically adds a new VM to state.
func (s *Store) AddVM(vm *VM) error {
	return s.Transact(func(st *State) error {
		if _, exists := st.VMs[vm.Name]; exists {
			return fmt.Errorf("microVM %q already exists", vm.Name)
		}
		st.VMs[vm.Name] = vm
		return nil
	})
}

// UpdateVM atomically modifies a VM in state.
func (s *Store) UpdateVM(name string, fn func(*VM)) error {
	return s.Transact(func(st *State) error {
		vm, ok := st.VMs[name]
		if !ok {
			return fmt.Errorf("no microVM named %q", name)
		}
		fn(vm)
		return nil
	})
}

// RemoveVM atomically deletes a VM from state.
func (s *Store) RemoveVM(name string) error {
	return s.Transact(func(st *State) error {
		delete(st.VMs, name)
		return nil
	})
}

// ListVMs returns all VMs.
func (s *Store) ListVMs() ([]*VM, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	vms := make([]*VM, 0, len(st.VMs))
	for _, vm := range st.VMs {
		vms = append(vms, vm)
	}
	return vms, nil
}

// NextNetIndex atomically finds and reserves the lowest unused network index
// by immediately writing a placeholder VM. Caller must update or remove it.
func (s *Store) NextNetIndex() (int, error) {
	st, err := s.Load()
	if err != nil {
		return 0, err
	}
	used := make(map[int]bool)
	for _, vm := range st.VMs {
		used[vm.NetIndex] = true
	}
	for i := 0; i < 62; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("IP address pool exhausted (max 62 VMs). Delete unused VMs with: mvm delete")
}

// ValidateName checks that a VM name is safe for shell use.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("VM name cannot be empty")
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return fmt.Errorf("VM name %q contains invalid character %q (use alphanumeric, hyphens, underscores, dots)", name, string(c))
		}
	}
	return nil
}

// ReserveVM atomically checks name uniqueness, allocates a net index, and saves
// the VM in one locked transaction. Returns the allocated NetIndex.
func (s *Store) ReserveVM(vm *VM) (int, error) {
	if err := ValidateName(vm.Name); err != nil {
		return 0, err
	}
	var idx int
	err := s.Transact(func(st *State) error {
		if _, exists := st.VMs[vm.Name]; exists {
			return fmt.Errorf("microVM %q already exists", vm.Name)
		}
		// Find lowest free net index
		used := make(map[int]bool)
		for _, v := range st.VMs {
			used[v.NetIndex] = true
		}
		found := false
		for i := 0; i < 62; i++ {
			if !used[i] {
				idx = i
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("IP address pool exhausted (max 62 VMs)")
		}
		vm.NetIndex = idx
		st.VMs[vm.Name] = vm
		return nil
	})
	return idx, err
}

// IsInitialized checks if mvm has been initialized.
func (s *Store) IsInitialized() (bool, error) {
	st, err := s.Load()
	if err != nil {
		return false, err
	}
	return st.Initialized, nil
}

// MarkInitialized marks the state as initialized.
func (s *Store) MarkInitialized(fcVersion, backend string) error {
	return s.Transact(func(st *State) error {
		st.Initialized = true
		st.InitAt = time.Now()
		st.FCVersion = fcVersion
		st.Backend = backend
		return nil
	})
}

// GetBackend returns the configured backend ("firecracker" or "applevz").
func (s *Store) GetBackend() string {
	st, err := s.Load()
	if err != nil || st.Backend == "" {
		return "firecracker" // default
	}
	return st.Backend
}
