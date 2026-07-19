package state

import "encoding/json"

// VMSpec is the declarative record of a VM create request. It is persisted
// verbatim at create time and returned by inspect — the "what the user asked
// for" companion to the runtime fields on VM. Templates and future
// declarative files serialize this same record.
type VMSpec struct {
	// Image is the reference as given at create time. Digest pinning
	// arrives with the OCI image store; no digest field until then.
	Image     string          `json:"image,omitempty"`
	Cpus      int             `json:"cpus,omitempty"`
	MemoryMB  int             `json:"memory_mb,omitempty"`
	Ports     []PortMap       `json:"ports,omitempty"`
	Volumes   []string        `json:"volumes,omitempty"`
	NetPolicy string          `json:"net_policy,omitempty"`
	Seccomp   string          `json:"seccomp,omitempty"`
	Secrets   []string        `json:"secrets,omitempty"`
	Startup   json.RawMessage `json:"startup,omitempty"`
}
