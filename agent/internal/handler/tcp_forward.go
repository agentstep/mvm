package handler

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandleTCPForward connects to a TCP port on the guest's own loopback and
// relays raw bytes between that connection and the control connection. After
// the OK frame, the stream is raw (no framing) in both directions — the host
// side pipes an inbound proxy/tunnel connection straight through.
//
// The dial target is hard-coded to 127.0.0.1 inside the guest: the request
// only carries a port, never a host, so this can't be turned into an SSRF
// primitive against the guest's LAN or anything else.
func HandleTCPForward(conn net.Conn, req *protocol.ForwardRequest) {
	if req == nil || req.Port < 1 || req.Port > 65535 {
		_ = protocol.WriteFrame(conn, &protocol.Response{Type: protocol.RespError, Error: "invalid forward port"})
		return
	}

	target := fmt.Sprintf("127.0.0.1:%d", req.Port)
	guest, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_ = protocol.WriteFrame(conn, &protocol.Response{Type: protocol.RespError, Error: fmt.Sprintf("dial %s: %v", target, err)})
		return
	}
	defer guest.Close()

	if err := protocol.WriteFrame(conn, &protocol.Response{Type: protocol.RespOK}); err != nil {
		return
	}

	// Raw bidirectional relay. When either side closes, tear down both so the
	// paired goroutine unblocks (CloseWrite isn't available on all conn types,
	// so we close hard).
	done := make(chan struct{}, 2)
	go func() { io.Copy(guest, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, guest); done <- struct{}{} }()
	<-done
}
