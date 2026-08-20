//go:build !linux

package handler

import (
	"fmt"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// doMount is a stub for non-Linux builds. The agent only ever runs inside the
// guest; this exists so the package compiles and ValidateMount stays testable
// on the host.
func doMount(req *protocol.MountRequest) error {
	return fmt.Errorf("mount is only supported inside a Linux guest")
}
