package handler

import (
	"os"
	"os/exec"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandleSetupNetwork configures the default route and DNS.
func HandleSetupNetwork(req *protocol.NetworkRequest) *protocol.Response {
	// Add default route
	if req.DefaultGateway != "" {
		cmd := exec.Command("ip", "route", "add", "default", "via", req.DefaultGateway, "dev", "eth0")
		cmd.Run() // ignore error if route already exists
	}

	// Set DNS
	if req.DNS != "" {
		os.WriteFile("/etc/resolv.conf", []byte("nameserver "+req.DNS+"\n"), 0o644)
	}

	return &protocol.Response{Type: protocol.RespOK}
}
