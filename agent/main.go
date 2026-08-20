package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/agentstep/mvm/agent/internal/container"
	"github.com/agentstep/mvm/agent/internal/handler"
	"github.com/agentstep/mvm/agent/internal/protocol"
)

const vsockPort = 5123

func main() {
	// Dispatch the inner-container init before anything else: this process was
	// re-exec'd into a fresh namespace and must not set up listeners, write the
	// ready file, or otherwise behave like the outer agent. Never returns.
	if container.IsInitProcess(os.Args) {
		container.RunInit(serveConn)
		return
	}

	log.SetPrefix("[mvm-agent] ")
	log.SetFlags(log.LstdFlags)

	// The agent is PID 1, so every orphaned process in the guest reparents to
	// it. Until this existed nothing ever called wait4, so those orphans became
	// permanent zombies — one per detached `mvm exec -d` job, for the life of
	// the VM. Must start before any connection is served so no exit is missed.
	go handler.ReapForever()

	// Dark launch: the inner container is created and supervised, but NOTHING is
	// routed to it yet. Handlers still run in the root namespace exactly as
	// before, so behaviour is unchanged; this exercises the spawn, the control
	// socket and the respawn path under real conditions before anything depends
	// on them. Failure is non-fatal for the same reason.
	if os.Getenv("MVM_NO_CONTAINER") == "" {
		cm := container.NewManager()
		if err := cm.Start(); err != nil {
			log.Printf("inner container unavailable (continuing without it): %v", err)
		} else {
			go cm.Supervise()
			// Only now is routing enabled. Until MVM_CONTAINER_EXEC is set,
			// user code still runs in the root namespace exactly as before —
			// the container is created and supervised but nothing is sent to
			// it, so the spawn and respawn paths are exercised in production
			// before anything depends on them.
			if os.Getenv("MVM_CONTAINER_EXEC") != "" {
				containerMgr = cm
				log.Printf("routing user code into the inner container")
			}
		}
	}

	// Start TCP listener immediately (legacy host-side control path).
	//
	// The port is overridable so a second agent can be run alongside the real
	// one for testing. The host always uses vsockPort; nothing in production
	// sets this.
	listenPort := vsockPort
	if p := os.Getenv("MVM_AGENT_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			listenPort = n
		}
	}
	tcpLn, tcpErr := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if tcpErr != nil {
		log.Fatalf("TCP listen failed: %v", tcpErr)
	}
	// The agent protocol is unauthenticated, and on the Firecracker/TAP backend
	// other VMs on the host can route to this guest's IP:5123. Restrict the TCP
	// port to the host (this guest's default gateway): legitimate host->guest
	// calls arrive from the gateway, cross-VM traffic arrives from a different
	// subnet. vsock (the primary path) has no IP and is unaffected. If the
	// gateway can't be determined the guest has no routing, so other VMs can't
	// reach it either — allow in that case to preserve functionality.
	//
	// This filter is the ONLY thing enforcing that restriction, so there must be
	// exactly one accept loop on tcpLn. There used to be two — this one, and the
	// main loop below via `ln = tcpLn` — racing for each connection, so roughly
	// half of all TCP connections were handed straight to handleConnection with
	// no gateway check at all. The main loop now blocks forever instead.
	gateway := handler.DefaultGatewayIP()
	go acceptTCP(tcpLn, gateway)

	// Try vsock in background — upgrade to vsock when driver is ready.
	// Poll with backoff: the vsock driver is usually probe-ready within tens
	// of ms of boot, so start tight (5ms) to bind ASAP, then back off toward
	// 250ms so a vsock-less guest doesn't busy-spin. Same ~15s overall budget.
	go acceptVsock()

	log.Printf("listening on port %d (vsock %d)", listenPort, vsockPort)
	os.WriteFile("/run/mvm-agent.ready", []byte("ready"), 0o644)

	// Both listeners are served by their own goroutine; park here. This used to
	// be a second accept loop on tcpLn, which is what created the race described
	// above.
	select {}
}

// acceptTCP serves the legacy TCP control path, admitting only connections from
// the guest's default gateway (the host). See the comment at the call site: this
// check is the only barrier against another VM on the same host reaching this
// guest's unauthenticated agent port, so it must not be bypassed.
//
// An empty gateway means the guest has no routing, in which case no other VM can
// reach it either, so connections are admitted to preserve functionality.
func acceptTCP(ln net.Listener, gateway string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		if gateway != "" {
			if ta, ok := conn.RemoteAddr().(*net.TCPAddr); !ok || ta.IP.String() != gateway {
				conn.Close()
				continue
			}
		}
		go handleConnection(conn)
	}
}

// acceptVsock waits for the vsock driver to appear, then serves the primary
// control path. vsock carries no IP, so it needs no peer filtering.
func acceptVsock() {
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
}

// containerMgr is the inner container, or nil if none is running. It is set
// once during startup in the outer agent and stays nil in the inner init, which
// is what stops a forwarded request from being forwarded again.
var containerMgr *container.Manager

func handleConnection(conn net.Conn) {
	serveConn(conn, nil)
}

// serveConn serves a connection. firstFrame, if non-nil, is a request frame
// already read from it — the case for a connection handed over from the outer
// agent, where the frame had to be read to make the routing decision.
func serveConn(conn net.Conn, firstFrame []byte) {
	defer conn.Close()

	for {
		var raw []byte
		if firstFrame != nil {
			raw, firstFrame = firstFrame, nil
		} else {
			var err error
			if raw, err = protocol.ReadRawFrame(conn); err != nil {
				return
			}
		}

		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			return
		}

		// Hand user-code requests to the inner container, passing the whole
		// connection rather than proxying it. On success the container owns the
		// connection and this loop is done; on failure we serve it here, so a
		// broken container degrades to previous behaviour instead of an outage.
		if containerMgr != nil && container.RouteInside(req.Type) {
			if err := containerMgr.Send(conn, raw); err == nil {
				return
			} else {
				log.Printf("routing %s to container failed, serving locally: %v", req.Type, err)
			}
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

		case protocol.ReqTCPForward:
			handler.HandleTCPForward(conn, req.Forward)
			return // forward takes over the connection (raw relay)

		case protocol.ReqNetInfo:
			resp = handler.HandleNetInfo()
			resp.ID = req.ID

		// Handled in the root namespace on purpose: the root mount tree is
		// rshared, so this propagates into the inner container and into any
		// container spawned later. Mounting inside the container instead would
		// confine it there and lose it on respawn.
		case protocol.ReqMount:
			resp = handler.HandleMount(req.Mount)
			resp.ID = req.ID

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
