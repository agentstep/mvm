// Package sdk provides a Go client for the mvm daemon API.
//
// This package is independent of any internal mvm packages and duplicates
// all request/response types so that external consumers can import it
// without pulling in mvm internals.
package sdk

// CreateVMRequest is the body for POST /vms.
type CreateVMRequest struct {
	Name      string    `json:"name"`
	Cpus      int       `json:"cpus,omitempty"`
	MemoryMB  int       `json:"memory_mb,omitempty"`
	Ports     []PortMap `json:"ports,omitempty"`
	NetPolicy string    `json:"net_policy,omitempty"`
	Volumes   []string  `json:"volumes,omitempty"`
	Seccomp   string    `json:"seccomp,omitempty"`
	Image     string    `json:"image,omitempty"`
}

// PortMap describes a host-to-guest port mapping.
type PortMap struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto,omitempty"`
}

// VMResponse is the API representation of a virtual machine.
type VMResponse struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	GuestIP   string    `json:"guest_ip,omitempty"`
	PID       int       `json:"pid,omitempty"`
	Backend   string    `json:"backend,omitempty"`
	Ports     []PortMap `json:"ports,omitempty"`
	CreatedAt string    `json:"created_at"`
	Error     string    `json:"error,omitempty"`
}

// ExecResult holds the output and exit code of a command execution.
type ExecResult struct {
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// SnapshotInfo describes a VM snapshot.
type SnapshotInfo struct {
	Name    string `json:"name"`
	VM      string `json:"vm,omitempty"`
	Created string `json:"created,omitempty"`
	Type    string `json:"type,omitempty"`
}

// BuildStep represents a single step in a build recipe.
type BuildStep struct {
	Directive string `json:"directive"`
	Args      string `json:"args"`
}

// ImageInfo describes a custom rootfs image.
type ImageInfo struct {
	Name   string `json:"name"`
	SizeMB int    `json:"size_mb"`
}

// PoolStatus holds warm pool counts.
type PoolStatus struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
}
