package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/agentstep/mvm/agent/internal/handler"
	"github.com/agentstep/mvm/agent/internal/protocol"
)

const vsockPort = 5123

func main() {
	log.SetPrefix("[mvm-agent] ")
	log.SetFlags(log.LstdFlags)

	// Start TCP listener immediately (for SSH tunnel connectivity)
	tcpLn, tcpErr := net.Listen("tcp", ":5123")
	if tcpErr != nil {
		log.Fatalf("TCP listen failed: %v", tcpErr)
	}
	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				continue
			}
			go handleConnection(conn)
		}
	}()

	// Try vsock in background — upgrade to vsock when driver is ready.
	// Poll with backoff: the vsock driver is usually probe-ready within tens
	// of ms of boot, so start tight (5ms) to bind ASAP, then back off toward
	// 250ms so a vsock-less guest doesn't busy-spin. Same ~15s overall budget.
	var ln net.Listener
	go func() {
		delay := 5 * time.Millisecond
		const maxDelay = 250 * time.Millisecond
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat("/sys/class/misc/vsock"); err == nil {
				if vsockLn, err := listenVsock(vsockPort); err == nil {
					log.Printf("vsock listener ready")
					for {
						conn, err := vsockLn.Accept()
						if err != nil {
							continue
						}
						go handleConnection(conn)
					}
				}
			}
			time.Sleep(delay)
			if delay < maxDelay {
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
			}
		}
		log.Printf("vsock not available, TCP-only mode")
	}()
	ln = tcpLn

	log.Printf("listening on port %d", vsockPort)
	os.WriteFile("/run/mvm-agent.ready", []byte("ready"), 0o644)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	for {
		var req protocol.Request
		if err := protocol.ReadFrame(conn, &req); err != nil {
			return
		}

		var resp *protocol.Response

		switch req.Type {
		case protocol.ReqPing:
			resp = &protocol.Response{Type: protocol.RespOK, ID: req.ID}

		case protocol.ReqExec:
			if req.Exec == nil {
				resp = &protocol.Response{Type: protocol.RespError, ID: req.ID, Error: "missing exec request"}
			} else {
				resp = handler.HandleExec(req.Exec)
				resp.ID = req.ID
			}

		case protocol.ReqExecStream:
			if req.Exec == nil {
				resp = &protocol.Response{Type: protocol.RespError, ID: req.ID, Error: "missing exec request"}
			} else {
				handler.HandleExecStream(conn, req.Exec, req.ID)
				continue
			}

		case protocol.ReqExecPty:
			if req.Pty == nil {
				resp = &protocol.Response{Type: protocol.RespError, ID: req.ID, Error: "missing pty request"}
			} else {
				handler.HandleExecPty(conn, req.Pty, req.ID)
				return // PTY takes over the connection
			}

		case protocol.ReqWriteFile:
			if req.File == nil {
				resp = &protocol.Response{Type: protocol.RespError, ID: req.ID, Error: "missing file request"}
			} else {
				resp = handler.HandleWriteFile(req.File)
				resp.ID = req.ID
			}

		case protocol.ReqReadFile:
			if req.File == nil {
				resp = &protocol.Response{Type: protocol.RespError, ID: req.ID, Error: "missing file request"}
			} else {
				resp = handler.HandleReadFile(req.File)
				resp.ID = req.ID
			}

		case protocol.ReqSetupNet:
			if req.Network == nil {
				resp = &protocol.Response{Type: protocol.RespError, ID: req.ID, Error: "missing network request"}
			} else {
				resp = handler.HandleSetupNetwork(req.Network)
				resp.ID = req.ID
			}

		case protocol.ReqPoweroff:
			resp = handler.HandlePoweroff()
			resp.ID = req.ID
			protocol.WriteFrame(conn, resp)
			return

		default:
			resp = &protocol.Response{Type: protocol.RespError, ID: req.ID, Error: fmt.Sprintf("unknown request type: %s", req.Type)}
		}

		if err := protocol.WriteFrame(conn, resp); err != nil {
			return
		}
	}
}
