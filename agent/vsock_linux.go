//go:build linux

package main

import (
	"net"

	"github.com/mdlayher/vsock"
)

func listenVsock(port int) (net.Listener, error) {
	return vsock.Listen(uint32(port), nil)
}
