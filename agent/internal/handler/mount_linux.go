//go:build linux

package handler

import (
	"os"
	"syscall"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// doMount performs a validated mount. Callers must have run ValidateMount.
func doMount(req *protocol.MountRequest) error {
	if req.MkDir {
		if err := os.MkdirAll(req.Target, 0o755); err != nil {
			return err
		}
	}
	return syscall.Mount(req.Source, req.Target, req.FSType, 0, req.Data)
}
