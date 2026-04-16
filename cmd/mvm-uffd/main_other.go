//go:build !linux

// The mvm-uffd binary is Linux-only. On other platforms we compile a stub
// main so that `go build ./...` and `go vet ./...` succeed on macOS CI. The
// stub exits with a clear error if someone attempts to run it.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mvm-uffd requires Linux (userfaultfd)")
	os.Exit(1)
}
