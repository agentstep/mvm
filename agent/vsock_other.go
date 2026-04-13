//go:build !linux

package main

import (
	"fmt"
	"net"
)

func listenVsock(port int) (net.Listener, error) {
	return nil, fmt.Errorf("vsock only available on Linux")
}
