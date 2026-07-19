package cli

import (
	"fmt"
	"time"
)

// waitForReady polls probe until it returns nil or timeout elapses,
// backing off between attempts (200ms, doubling, capped at 2s). Bridges the
// gap between a VM reporting "running" and its guest agent actually being
// reachable — neither backend's create path blocks on that today (Firecracker
// readiness happens in a daemon-side goroutine; see
// internal/server/routes.go's handleCreateVM).
func waitForReady(timeout time.Duration, probe func() error) error {
	deadline := time.Now().Add(timeout)
	backoff := 200 * time.Millisecond
	var lastErr error
	for {
		if err := probe(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for VM to become ready: %w", lastErr)
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// resolveImage maps the "base" sentinel to the implicit default rootfs
// (image="", exactly what runStart/handleCreateVM already treat as "use
// base.ext4"). "base" is not yet a real catalogued image — that arrives
// with the OCI image store (design spec step 3) — so this mapping lives
// entirely at the CLI layer.
func resolveImage(image string) string {
	if image == "base" {
		return ""
	}
	return image
}

// resolveRunName decides the VM's name and whether it should be deleted
// after its foreground command exits. An explicit --name opts into
// durability (never auto-deleted); with no --name, a fresh name is
// generated and the VM is ephemeral.
func resolveRunName(nameFlag string, existing map[string]bool) (name string, ephemeral bool) {
	if nameFlag != "" {
		return nameFlag, false
	}
	return GenerateVMName(existing), true
}
