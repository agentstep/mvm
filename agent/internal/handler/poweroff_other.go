//go:build !linux

package handler

import "fmt"

// powerOff is a stub for non-Linux builds. The agent only ever runs inside the
// guest (linux/arm64); this exists so the package still builds on macOS, where
// the host-side tests compile it.
func powerOff() error {
	return fmt.Errorf("poweroff is only supported inside a Linux guest")
}
