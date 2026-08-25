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
	Services  []Service       `json:"services,omitempty"`
	// IdleTimeout / ArchiveAfter are the idle-tiering thresholds as requested at
	// create time. The live values on state.VM are what the sweep reads; these are
	// the declaration, kept so inspect shows what was asked for.
	IdleTimeout  string `json:"idle_timeout,omitempty"`
	ArchiveAfter string `json:"archive_after,omitempty"`
}

// Service is a long-running process mvm keeps alive inside a VM.
//
// Persisted in VMSpec so it survives the VM: the agent's registry is live state
// that dies with the guest, while this is the declaration of what *should* be
// running. Reconciliation is one-directional — the declared list wins.
type Service struct {
	Name    string            `json:"name"`
	Run     string            `json:"run"`
	WorkDir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Restart is "always" (default), "on-failure", or "never".
	Restart string `json:"restart,omitempty"`
}

// RestartPolicy values.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// ShouldRestart reports whether a service that exited with the given code
// should be restarted.
func (s Service) ShouldRestart(exitCode int) bool {
	switch s.Restart {
	case RestartNever:
		return false
	case RestartOnFailure:
		return exitCode != 0
	default: // RestartAlways, and the empty default
		return true
	}
}

// ValidServiceName reports whether a name is safe to use as a map key and to
// echo in logs and CLI output. Same character class as VM names.
func ValidServiceName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}
