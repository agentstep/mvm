//go:build linux

package container

import (
	"fmt"
	"log"
	"net"
	"os"
	"syscall"

	"github.com/agentstep/mvm/agent/internal/handler"
)

// ctrlFD is where the control socketpair arrives in the inner init. Go's
// ExtraFiles places the first extra file at fd 3.
const ctrlFD = 3

// Dispatcher serves one connection, starting from a request frame that has
// already been read from it.
//
// The inner init needs the agent's full request dispatch, which lives in
// package main; passing it in as a callback keeps that dispatch table in one
// place rather than duplicating it here and letting the two drift.
type Dispatcher func(conn net.Conn, firstFrame []byte)

// RunInit is the entry point for the inner-namespace init. main() dispatches
// here when IsInitProcess(os.Args) is true, before doing anything else.
//
// This process is PID 1 in its namespace, which carries obligations the outer
// agent does not have: it must reap, and it must not die, because when a PID
// namespace's PID 1 exits the kernel SIGKILLs every other process in it and the
// namespace can never be used again.
//
// It never returns.
func RunInit(dispatch Dispatcher) {
	log.SetPrefix("[mvm-container] ")
	log.SetFlags(log.LstdFlags)

	if pid := os.Getpid(); pid != 1 {
		// Not fatal — the namespace may be unavailable on this kernel and the
		// caller falls back — but it means we are not the init we think we are.
		log.Printf("warning: container init is pid %d, expected 1", pid)
	}

	// Re-set FD_CLOEXEC on the inherited control socket. Go clears it for
	// ExtraFiles, so without this every process spawned in here inherits the
	// channel: EOF-based death detection in the outer agent stops working
	// (a lingering user fd holds the socket open), and sandboxed user code
	// gets a writable path to the outer agent's control plane.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, ctrlFD, syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 {
		log.Printf("warning: could not set FD_CLOEXEC on control fd: %v", errno)
	}

	// Hold a reference for the process lifetime: a garbage-collected os.File
	// closes its fd in a finalizer, which would drop the control channel at an
	// unpredictable moment.
	ctrl := os.NewFile(ctrlFD, "container-ctrl")
	defer func() { _ = ctrl }()

	if err := setupMounts(); err != nil {
		log.Printf("warning: container mount setup: %v", err)
	}

	// PID 1 must reap, and this namespace's orphans reparent here rather than
	// to the outer agent.
	go handler.ReapForever()

	log.Printf("container init ready (%s)", Describe())

	unixCtrl, err := ctrlConn(ctrl)
	if err != nil {
		// Without the control channel nothing can ever be routed here, but
		// exiting would tear down the namespace and trigger a respawn loop.
		// Park instead and let the outer agent serve everything itself.
		log.Printf("control channel unusable (%v); no work will be routed here", err)
		select {}
	}

	// Serve connections handed over by the outer agent. Each arrives as a file
	// descriptor plus the request frame already read from it, so the existing
	// dispatch runs verbatim — the connection is owned outright rather than
	// proxied, which is why PTY and streaming work here without special cases.
	for {
		conn, rawReq, err := RecvConn(unixCtrl)
		if err != nil {
			// EOF means the outer agent is gone. Exiting takes the namespace
			// down with us, which is correct: there is nobody left to serve.
			log.Printf("control channel closed (%v); container init exiting", err)
			return
		}
		go dispatch(conn, rawReq)
	}
}

// ctrlConn wraps the inherited control fd as a *net.UnixConn.
func ctrlConn(f *os.File) (*net.UnixConn, error) {
	fc, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	uc, ok := fc.(*net.UnixConn)
	if !ok {
		fc.Close()
		return nil, fmt.Errorf("control channel is %T, want *net.UnixConn", fc)
	}
	return uc, nil
}

// setupMounts gives the container its own view of the pseudo-filesystems whose
// contents are namespace-scoped.
//
// Mount propagation is set to rslave first: the outer namespace is made
// rshared by mvm-init, so mounts the outer agent performs later (volumes, in
// particular) propagate inward, while mounts made in here do not leak back out.
// Without this, applevz volume mounts — which are performed post-boot via agent
// exec — would land only in whichever namespace happened to run them and would
// silently vanish on a respawn.
func setupMounts() error {
	if err := syscall.Mount("", "/", "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return err
	}
	// A private /proc so `ps` inside the container sees only container
	// processes. Without this, /proc still shows the whole guest.
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		log.Printf("warning: mount /proc: %v", err)
	}
	// devpts: a fresh instance means PTYs allocated in here get their own
	// numbering and do not collide with the outer namespace's.
	mountAt("/dev/pts", 0o755, "devpts", "devpts", 0, "mode=620,ptmxmode=666")

	// /dev/shm is capped well below the outer instance's 50%: two tmpfs each
	// allowed half of RAM could jointly consume all of it, which defeats the
	// point of capping either.
	mountAt("/dev/shm", 0o1777, "tmpfs", "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NODEV, "size=25%,mode=1777")

	return nil
}

// mountAt mounts source at target, creating target first if it is missing.
//
// The mkdir matters: /dev is devtmpfs, so a target that the guest image does
// not ship simply does not exist, and mount fails with ENOENT. Guests built
// before /dev/shm was added to mvm-init hit exactly that. Creating it keeps the
// container working on older images instead of silently starting without shared
// memory — which is a fatal condition for anything using shm_open (Chromium
// aborts outright).
func mountAt(target string, perm os.FileMode, source, fstype string, flags uintptr, data string) {
	if err := os.MkdirAll(target, perm); err != nil {
		log.Printf("warning: mkdir %s: %v", target, err)
		return
	}
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil {
		log.Printf("warning: mount %s: %v", target, err)
	}
}
