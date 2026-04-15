package cli

import (
	"fmt"

	"github.com/agentstep/mvm/internal/server"
)

// requireDaemon returns a daemon client or an error if the daemon is not running.
// All Firecracker CLI commands should call this instead of accessing state/Lima directly.
func requireDaemon() (*server.Client, error) {
	sc := server.DefaultClient()
	if !sc.IsAvailable() {
		return nil, fmt.Errorf("daemon not running. Start it with: mvm serve start")
	}
	return sc, nil
}
