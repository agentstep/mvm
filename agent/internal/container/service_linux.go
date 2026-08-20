//go:build linux

package container

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// Service is a long-running process the supervisor keeps alive.
type Service struct {
	Name    string            `json:"name"`
	Run     string            `json:"run"`
	WorkDir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Restart string            `json:"restart,omitempty"` // always | on-failure | never
}

func (s Service) shouldRestart(exitCode int) bool {
	switch s.Restart {
	case "never":
		return false
	case "on-failure":
		return exitCode != 0
	default:
		return true
	}
}

// ServiceState is one service as reported by List.
type ServiceState struct {
	Service
	Running  bool `json:"running"`
	Restarts int  `json:"restarts"`
	LastExit int  `json:"last_exit"`
}

// Supervisor keeps declared services running inside the inner container.
//
// It lives in the ROOT namespace, deliberately. That is the whole reason the
// inner container exists: a supervisor inside the namespace it supervises dies
// with it, so nothing could restart user code after a bounce. From out here it
// survives, notices, and restarts.
//
// Services are started by reusing the connection-handoff path rather than
// adding a second way to spawn into the container: the supervisor makes a
// socketpair, sends one end in with a synthetic exec_stream request, and reads
// the exit frame off the other end. When that frame arrives the service has
// died, and the restart policy decides what happens next. No inner-side code is
// involved at all.
type Supervisor struct {
	mgr *Manager

	mu       sync.Mutex
	services map[string]*supervised
}

type supervised struct {
	svc      Service
	running  bool
	restarts int
	lastExit int
	stop     chan struct{}
	logs     *logBuffer
}

// NewSupervisor returns a supervisor driving the given container.
func NewSupervisor(mgr *Manager) *Supervisor {
	return &Supervisor{mgr: mgr, services: map[string]*supervised{}}
}

// Add registers a service and starts supervising it. Replaces any existing
// service of the same name, so re-declaring converges rather than duplicating.
func (s *Supervisor) Add(svc Service) error {
	if svc.Name == "" || svc.Run == "" {
		return fmt.Errorf("service needs a name and a command")
	}
	s.Remove(svc.Name)

	rec := &supervised{svc: svc, stop: make(chan struct{}), logs: newLogBuffer()}
	s.mu.Lock()
	s.services[svc.Name] = rec
	s.mu.Unlock()

	go s.supervise(rec)
	return nil
}

// Remove stops supervising a service and terminates it.
func (s *Supervisor) Remove(name string) {
	s.mu.Lock()
	rec, ok := s.services[name]
	delete(s.services, name)
	s.mu.Unlock()
	if ok {
		close(rec.stop)
	}
}

// List reports every supervised service.
func (s *Supervisor) List() []ServiceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ServiceState, 0, len(s.services))
	for _, rec := range s.services {
		out = append(out, ServiceState{
			Service:  rec.svc,
			Running:  rec.running,
			Restarts: rec.restarts,
			LastExit: rec.lastExit,
		})
	}
	return out
}

// Logs returns the most recent output of one service. n <= 0 means everything
// retained.
//
// Logs survive a restart of the service and a bounce of the container: the
// buffer belongs to the supervisor in the root namespace, not to the process,
// so the output explaining why something died is still there afterwards. That
// is the whole reason this lives out here.
func (s *Supervisor) Logs(name string, n int) ([]LogLine, error) {
	s.mu.Lock()
	rec, ok := s.services[name]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no service named %q", name)
	}
	return rec.logs.Tail(n), nil
}

// Restart forces one service to restart now.
func (s *Supervisor) Restart(name string) error {
	s.mu.Lock()
	rec, ok := s.services[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no service named %q", name)
	}
	// Add() replaces the record, so carry the log buffer over — the output
	// explaining why a service needed restarting is exactly what someone will
	// look for immediately afterwards.
	prev := rec.logs
	if err := s.Add(rec.svc); err != nil {
		return err
	}
	s.mu.Lock()
	if fresh, ok := s.services[name]; ok {
		fresh.logs = prev
	}
	s.mu.Unlock()
	return nil
}

// Reconcile brings the container in line with a declared service list.
//
// Idempotent by design, so one call serves boot, resume, restore and bounce:
// anything declared but not running is started, anything running but no longer
// declared is stopped. On resume it finds everything already up and does
// nothing; after a bounce it finds nothing and starts everything.
func (s *Supervisor) Reconcile(declared []Service) {
	want := make(map[string]Service, len(declared))
	for _, svc := range declared {
		want[svc.Name] = svc
	}

	s.mu.Lock()
	var toStop []string
	for name := range s.services {
		if _, ok := want[name]; !ok {
			toStop = append(toStop, name)
		}
	}
	running := make(map[string]bool, len(s.services))
	for name, rec := range s.services {
		running[name] = rec.running
	}
	s.mu.Unlock()

	for _, name := range toStop {
		log.Printf("service %s: no longer declared, stopping", name)
		s.Remove(name)
	}
	for name, svc := range want {
		if !running[name] {
			log.Printf("service %s: declared but not running, starting", name)
			s.Add(svc)
		}
	}
}

// supervise runs one service until it is removed, restarting per policy.
func (s *Supervisor) supervise(rec *supervised) {
	const (
		minBackoff = 200 * time.Millisecond
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff

	for {
		select {
		case <-rec.stop:
			return
		default:
		}

		exit, err := s.runOnce(rec)
		if err != nil {
			log.Printf("service %s: could not start: %v", rec.svc.Name, err)
		} else {
			log.Printf("service %s: exited with %d", rec.svc.Name, exit)
		}

		s.mu.Lock()
		rec.running = false
		rec.lastExit = exit
		s.mu.Unlock()

		select {
		case <-rec.stop:
			return
		default:
		}

		if err == nil && !rec.svc.shouldRestart(exit) {
			log.Printf("service %s: restart policy %q, not restarting", rec.svc.Name, rec.svc.Restart)
			return
		}

		// Backoff so a service that cannot start does not spin, burning CPU and
		// burying the reason in log spam.
		select {
		case <-rec.stop:
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}

		s.mu.Lock()
		rec.restarts++
		s.mu.Unlock()
	}
}

// runOnce starts the service in the container and blocks until it exits.
func (s *Supervisor) runOnce(rec *supervised) (int, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socketpair: %w", err)
	}

	ours := newFDConn(fds[0], "service-local")
	theirs := newFDConn(fds[1], "service-remote")
	defer ours.Close()

	req := protocol.Request{
		Type: protocol.ReqExecStream,
		ID:   "svc-" + rec.svc.Name,
		Exec: &protocol.ExecRequest{
			Command: rec.svc.Run,
			Env:     rec.svc.Env,
			WorkDir: rec.svc.WorkDir,
		},
	}
	raw, err := json.Marshal(&req)
	if err != nil {
		theirs.Close()
		return -1, err
	}

	// Hand the far end into the container. On success the container owns it, so
	// release our reference to that side.
	if err := s.mgr.Send(theirs, raw); err != nil {
		theirs.Close()
		return -1, err
	}
	theirs.Close()

	s.mu.Lock()
	rec.running = true
	s.mu.Unlock()

	// Drain frames until the exit frame, capturing output as it arrives. The
	// buffer is bounded, so a service that logs in a loop cannot grow without
	// limit — which matters because the rootfs is the VM's durable state and
	// filling it would break far more than logging.
	for {
		var resp protocol.Response
		if err := protocol.ReadFrame(ours, &resp); err != nil {
			// The container went away (bounce, crash). Treat as a non-zero
			// exit so the restart policy applies.
			return -1, nil
		}
		switch resp.Type {
		case protocol.RespStdout:
			rec.logs.Append("stdout", resp.Data)
		case protocol.RespStderr:
			rec.logs.Append("stderr", resp.Data)
		case protocol.RespExit:
			return resp.ExitCode, nil
		}
	}
}

// newFDConn wraps a raw socket fd as a net.Conn without going through
// net.FileConn, which cannot resolve every socket family.
func newFDConn(fd int, name string) net.Conn {
	return &fdConn{File: os.NewFile(uintptr(fd), name)}
}
