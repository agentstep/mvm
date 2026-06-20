//go:build !darwin

// Package menubar implements the macOS menu-bar UI. It is only built on
// darwin (it links Apple's Cocoa/Objective-C via github.com/caseymrm/menuet).
// This file keeps the package present-but-empty on other platforms so that
// `go build ./...` and `go vet ./...` don't fail with "build constraints
// exclude all Go files" on Linux CI.
package menubar
