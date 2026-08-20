//go:build linux

package container

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"syscall"
)

// handoff is the control message accompanying a passed connection fd. It
// carries the request frame the outer agent already consumed in order to make
// the routing decision, so the inner init can act on it without the outer
// agent having to un-read it.
type handoff struct {
	Request json.RawMessage `json:"request"`
}

// maxHandoffBytes bounds a control message. Handoffs carry one request frame,
// which is small; anything larger is a malformed or hostile peer.
const maxHandoffBytes = 1 << 20

// SendConn passes an accepted connection to the inner init, along with the
// request frame already read from it.
//
// The connection is handed over whole rather than proxied. Multiplexing
// sessions over the control socket was the obvious design and is wrong: exec_pty
// takes over its connection for the session's lifetime and tcp_forward switches
// to a raw unframed relay, so carrying them would mean inventing per-channel
// framing and flow control, with head-of-line blocking on interactive
// keystrokes. Passing the fd sidesteps all of it — and means the inner init
// opens the PTY master itself, in the same process that spawns the child, so
// Setctty works unchanged.
func SendConn(ctrl *net.UnixConn, conn net.Conn, rawRequest []byte) error {
	sysConn, ok := conn.(syscall.Conn)
	if !ok {
		return fmt.Errorf("connection type %T cannot be passed", conn)
	}
	rc, err := sysConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("syscall conn: %w", err)
	}

	payload, err := json.Marshal(handoff{Request: rawRequest})
	if err != nil {
		return fmt.Errorf("marshal handoff: %w", err)
	}

	var sendErr error
	if err := rc.Control(func(fd uintptr) {
		rights := syscall.UnixRights(int(fd))
		_, _, sendErr = ctrl.WriteMsgUnix(payload, rights, nil)
	}); err != nil {
		return fmt.Errorf("control fd: %w", err)
	}
	if sendErr != nil {
		return fmt.Errorf("send fd: %w", sendErr)
	}
	return nil
}

// RecvConn receives one passed connection and its request frame.
//
// Every received fd has FD_CLOEXEC set immediately. SCM_RIGHTS delivers fds
// without it, so without this any process spawned afterwards inherits the
// caller's live connections — leaking one user's session fd into another user's
// process, and keeping connections open past their session.
func RecvConn(ctrl *net.UnixConn) (net.Conn, []byte, error) {
	payload := make([]byte, maxHandoffBytes)
	oob := make([]byte, syscall.CmsgSpace(4)) // exactly one fd

	n, oobn, _, _, err := ctrl.ReadMsgUnix(payload, oob)
	if err != nil {
		return nil, nil, err
	}

	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, nil, fmt.Errorf("parse control message: %w", err)
	}
	if len(scms) != 1 {
		return nil, nil, fmt.Errorf("got %d control messages, want 1", len(scms))
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return nil, nil, fmt.Errorf("parse rights: %w", err)
	}
	if len(fds) != 1 {
		// Close whatever did arrive rather than leaking descriptors.
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return nil, nil, fmt.Errorf("got %d fds, want 1", len(fds))
	}

	fd := fds[0]
	syscall.CloseOnExec(fd)

	f := os.NewFile(uintptr(fd), "passed-conn")
	if f == nil {
		syscall.Close(fd)
		return nil, nil, fmt.Errorf("fd %d is not valid", fd)
	}
	// FileConn dups the descriptor, so the original must be closed or it leaks
	// for the life of the process.
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("wrap passed fd: %w", err)
	}

	var h handoff
	if err := json.Unmarshal(payload[:n], &h); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("decode handoff: %w", err)
	}
	return conn, h.Request, nil
}
