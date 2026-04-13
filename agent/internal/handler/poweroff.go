package handler

import (
	"os/exec"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandlePoweroff shuts down the system.
func HandlePoweroff() *protocol.Response {
	// Use poweroff command (works on Alpine)
	exec.Command("poweroff").Start()
	return &protocol.Response{Type: protocol.RespOK}
}
