package state

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// VM represents a single microVM's state.
type VM struct {
	Name         string     `json:"name"`
	Status       string     `json:"status"` // "running", "paused", "stopped"
	GuestIP      string     `json:"guest_ip"`
	TAPIP        string     `json:"tap_ip"`
	TAPDevice    string     `json:"tap_device"`
	GuestMAC     string     `json:"guest_mac"`
	NetIndex     int        `json:"net_index"`
	SocketPath   string     `json:"socket_path"`
	PID          int        `json:"pid"`
	UFFDPid      int        `json:"uffd_pid,omitempty"` // mvm-uffd sidecar PID (0 = File backend)
	RootfsPath   string     `json:"rootfs_path"`
	Ports        []PortMap  `json:"ports,omitempty"`
	NetPolicy    string     `json:"net_policy,omitempty"`   // "open", "deny", "allow:<domains>"
	Backend      string     `json:"backend,omitempty"`      // "firecracker" or "applevz"
	Cpus         int        `json:"cpus,omitempty"`         // vCPU count (0 = default)
	MemoryMB     int        `json:"memory_mb,omitempty"`    // RAM in MiB (0 = default)
	Secrets      []string   `json:"secrets,omitempty"`      // attached secret names (injected per-exec)
	IdleTimeout  string     `json:"idle_timeout,omitempty"` // e.g. "5m"
	LastActivity *time.Time `json:"last_activity,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	StoppedAt    *time.Time `json:"stopped_at,omitempty"`
	ForwarderPID int        `json:"forwarder_pid,omitempty"` // applevz: PID of the detached -p port-forwarder process, 0 if none
	Spec         *VMSpec    `json:"spec,omitempty"`          // declarative create request, returned by inspect
}

// PortMap represents a host:guest port forwarding rule.
type PortMap struct {
	HostIP    string `json:"host_ip,omitempty"` // bind address on the host; "" = the backend's existing default (see parsePorts)
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"` // "tcp" or "udp"
}

// ValidatePort rejects any PortMap whose fields could break out of their
// intended use. This is load-bearing for security, not just hygiene: on the
// Firecracker backend HostIP and Proto are interpolated into a `sudo iptables`
// command string that the daemon runs via `bash -c` (see
// internal/firecracker/network.go's SetupPortForwarding). An unvalidated
// HostIP like "$(cmd)" would be arbitrary root code execution on the daemon
// host. Both the CLI (parsePorts) and the daemon (handleCreateVM — the real
// trust boundary, since a remote client can POST any request body) call this.
func ValidatePort(p PortMap) error {
	if p.HostIP != "" && net.ParseIP(p.HostIP) == nil {
		return fmt.Errorf("invalid host IP %q (must be a valid IP address)", p.HostIP)
	}
	switch p.Proto {
	case "", "tcp", "udp":
	default:
		return fmt.Errorf("invalid protocol %q (must be tcp or udp)", p.Proto)
	}
	if p.HostPort < 1 || p.HostPort > 65535 {
		return fmt.Errorf("invalid host port %d (must be 1-65535)", p.HostPort)
	}
	if p.GuestPort < 1 || p.GuestPort > 65535 {
		return fmt.Errorf("invalid guest port %d (must be 1-65535)", p.GuestPort)
	}
	return nil
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
	if reservedNames[name] {
		return fmt.Errorf("VM name %q is reserved", name)
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return fmt.Errorf("VM name %q contains invalid character %q (use alphanumeric, hyphens, underscores, dots)", name, string(c))
		}
	}
	return nil
}

// reservedNames are VM names that would collide with literal path segments
// in the daemon's route table (e.g. "GET /vms/stats" takes precedence over
// "GET /vms/{name}" in net/http's ServeMux), causing a VM's own name to
// route to the wrong handler. "stats" collides with the real /vms/stats
// route; "health" is reserved pre-emptively since a future /vms/health
// route would have the identical failure mode.
var reservedNames = map[string]bool{
	"stats":  true,
	"health": true,
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

// GetBackend returns the configured backend ("firecracker" or "applevz"),
// defaulting to "firecracker" both when unset AND when the state file
// fails to load (e.g. a transient I/O error). Safe for call sites where a
// wrong guess has no real consequence — a self-healing periodic check
// (idle.go), a dev-only harness gate (bench.go), or a diagnostic printout
// (doctor.go). For any call site that gates a decision with real
// consequences — skipping a validation, or dispatching to a different
// code path entirely — use GetBackendE instead, so a load error surfaces
// rather than silently resolving to "firecracker". See
// internal/cli/run.go's applevz custom-image guard for the migrated
// example, and this file's package-level notes in the hardening plan
// (docs/superpowers/plans/2026-07-19-hardening-polish.md) for why the
// other GetBackend() call sites were deliberately left as-is.
func (s *Store) GetBackend() string {
	backend, err := s.GetBackendE()
	if err != nil {
		return "firecracker" // default
	}
	return backend
}

// GetBackendE is GetBackend's error-returning counterpart: it returns the
// configured backend, or propagates the underlying Load error instead of
// papering over it with the "firecracker" default.
func (s *Store) GetBackendE() (string, error) {
	st, err := s.Load()
	if err != nil {
		return "", err
	}
	if st.Backend == "" {
		return "firecracker", nil
	}
	return st.Backend, nil
}
