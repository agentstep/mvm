//go:build linux

package container

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Manager owns the inner container: it spawns the inner init, watches for its
// death, and respawns it.
//
// The respawn path is deliberately the same machinery a future `bounce` verb
// needs. When a PID namespace's PID 1 dies the kernel SIGKILLs everything in it
// and the namespace becomes permanently unusable, so recovery is always
// "create a fresh namespace", never "repair this one" — which is exactly what
// bouncing is.
type Manager struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	ctrl     *os.File // our end of the socketpair; held to keep the fd alive
	conn     *net.UnixConn
	restarts int
	running  bool
}

// NewManager returns a manager with no container started.
func NewManager() *Manager { return &Manager{} }

// Start creates the inner container. Safe to call again after the previous one
// died; it replaces any dead state.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked()
}

func (m *Manager) startLocked() error {
	// SOCK_SEQPACKET preserves message boundaries, so a control message is
	// never split or coalesced the way it would be on a stream socket. We pass
	// connection fds over this later via SCM_RIGHTS, which needs AF_UNIX.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socketpair: %w", err)
	}

	parentFile := os.NewFile(uintptr(fds[0]), "container-ctrl-parent")
	childFile := os.NewFile(uintptr(fds[1]), "container-ctrl-child")
	defer childFile.Close() // the child keeps its own copy after fork

	path, args := InitCommand()
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: CloneFlags(),
		// Without this the inner init survives an outer-agent crash as an
		// orphaned namespace nothing can reach or clean up.
		Pdeathsig: syscall.SIGKILL,
	}
	// Arrives as fd 3 in the child. NOTE: Go clears FD_CLOEXEC on ExtraFiles,
	// so the child MUST re-set it before spawning anything — otherwise every
	// user process inherits the control channel, which both breaks EOF-based
	// death detection and hands sandboxed code a writable path to the outer
	// agent. See initRun.
	cmd.ExtraFiles = []*os.File{childFile}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		parentFile.Close()
		return fmt.Errorf("spawn inner init: %w", err)
	}

	fc, err := net.FileConn(parentFile)
	if err != nil {
		parentFile.Close()
		cmd.Process.Kill()
		return fmt.Errorf("wrap control socket: %w", err)
	}
	unixConn, ok := fc.(*net.UnixConn)
	if !ok {
		fc.Close()
		cmd.Process.Kill()
		return fmt.Errorf("control socket is %T, want *net.UnixConn", fc)
	}

	m.cmd = cmd
	// Keep parentFile referenced for the container's lifetime: a garbage
	// collected os.File runs a finalizer that closes the underlying fd, which
	// would show up as an unreproducible control-channel drop.
	m.ctrl = parentFile
	m.conn = unixConn
	m.running = true

	log.Printf("inner container started (pid %d, %s)", cmd.Process.Pid, Describe())
	return nil
}

// Supervise blocks, restarting the inner container whenever it dies.
//
// Restarts are rate-limited: a container that cannot start stays broken, and
// respawning it in a tight loop would bury the reason in log spam and burn CPU
// for the life of the VM.
func (m *Manager) Supervise() {
	const (
		minBackoff = 200 * time.Millisecond
		maxBackoff = 10 * time.Second
	)
	backoff := minBackoff

	for {
		m.mu.Lock()
		cmd := m.cmd
		m.mu.Unlock()
		if cmd == nil {
			return
		}

		err := cmd.Wait()

		m.mu.Lock()
		m.running = false
		m.restarts++
		n := m.restarts
		if m.conn != nil {
			m.conn.Close()
		}
		if m.ctrl != nil {
			m.ctrl.Close()
		}
		m.mu.Unlock()

		log.Printf("inner container exited (%v); respawning (restart #%d)", err, n)
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}

		if err := m.Start(); err != nil {
			log.Printf("respawn failed: %v", err)
			continue
		}
		backoff = minBackoff
	}
}

// Send hands an accepted connection to the inner container, along with the
// request frame the outer agent already read from it.
//
// On success the container owns the connection: the caller must stop using it
// and release its own reference (the fd stays alive via the in-flight
// SCM_RIGHTS message even if the sender closes first). On failure the caller
// still owns it and should serve the request itself.
func (m *Manager) Send(conn net.Conn, rawRequest []byte) error {
	m.mu.Lock()
	ctrl, running := m.conn, m.running
	m.mu.Unlock()

	if !running || ctrl == nil {
		return fmt.Errorf("inner container is not running")
	}
	return SendConn(ctrl, conn, rawRequest)
}

// Running reports whether a container is currently up.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Restarts reports how many times the container has been respawned.
func (m *Manager) Restarts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restarts
}
