package handler

import (
	"fmt"
	"path"
	"strings"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// ValidateMount checks a mount request before any of its fields reach the
// kernel.
//
// Target becomes a mount point, so a traversal or parent reference could place
// a filesystem somewhere the caller never named — a real risk inherited from
// the previous approach, which built `mkdir -p X && mount -t virtiofs tag X` as
// a shell string and executed it.
func ValidateMount(req *protocol.MountRequest) error {
	if req == nil {
		return fmt.Errorf("missing mount request")
	}
	if req.Source == "" {
		return fmt.Errorf("mount source is required")
	}
	if req.FSType == "" {
		return fmt.Errorf("mount fstype is required")
	}
	if req.Target == "" {
		return fmt.Errorf("mount target is required")
	}
	if !strings.HasPrefix(req.Target, "/") {
		return fmt.Errorf("mount target %q must be an absolute path", req.Target)
	}
	// path.Clean resolves ".." lexically; if cleaning changes the path, it
	// contained traversal or redundant segments and is not what the caller
	// literally asked for.
	if cleaned := path.Clean(req.Target); cleaned != req.Target {
		return fmt.Errorf("mount target %q must be a clean absolute path (got %q after cleaning)", req.Target, cleaned)
	}
	if req.Target == "/" {
		return fmt.Errorf("refusing to mount over /")
	}
	return nil
}

// HandleMount mounts a filesystem in the guest.
//
// This runs in the ROOT namespace, deliberately. The root mount tree is
// rshared (see mvm-init), so a mount performed here propagates into the current
// inner container and into any container spawned later. Performing it inside
// the container instead would confine it to that namespace, and it would vanish
// on respawn — leaving an empty directory where a volume used to be, with no
// error anywhere.
func HandleMount(req *protocol.MountRequest) *protocol.Response {
	if err := ValidateMount(req); err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}
	if err := doMount(req); err != nil {
		return &protocol.Response{
			Type:  protocol.RespError,
			Error: fmt.Sprintf("mount %s at %s: %v", req.Source, req.Target, err),
		}
	}
	return &protocol.Response{Type: protocol.RespOK}
}
