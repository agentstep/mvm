//go:build !darwin

package main

import "fmt"

// The mvm-menu menu-bar app is macOS-only (it links Cocoa via menuet). This
// stub lets the command build on other platforms so `go build ./...` and
// `go vet ./...` pass on Linux CI.
func main() {
	fmt.Println("mvm-menu (menu-bar app) is only available on macOS")
}
