package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
)

// DaemonSocketPath is where the daemon inside Lima listens.
const DaemonSocketPath = "/run/mvm/daemon.sock"

// DaemonTCPPort is the TCP port the daemon also listens on inside Lima.
// SSH forwards this to macOS localhost for CLI connectivity.
const DaemonTCPPort = 19876

type Server struct {
	store        *state.Store
	executor     firecracker.Executor
	unixListener net.Listener
	tcpListener  net.Listener  // nil if no ListenAddr
	unixServer   *http.Server
	tcpServer    *http.Server  // nil if no ListenAddr
	sockPath     string
	pidPath      string
	cfg          Config
}

type Config struct {
	SocketPath string
	PIDPath    string
	Store      *state.Store
	Executor   firecracker.Executor
	ListenAddr string // TCP address, e.g. "0.0.0.0:19876"
	TLSCert    string
	TLSKey     string
	APIKey     string
}

func DefaultSocketPath() string {
	if IsLinux() {
		return DaemonSocketPath
	}
	// On macOS: use Lima's forwarded socket
	home, _ := os.UserHomeDir()
	limaForwarded := filepath.Join(home, ".lima", "mvm", "sock", "daemon.sock")
	if _, err := os.Stat(limaForwarded); err == nil {
		return limaForwarded
	}
	// Fallback: local socket (daemon running on macOS)
	return filepath.Join(home, ".mvm", "server.sock")
}

func DefaultPIDPath() string {
	if IsLinux() {
		return "/run/mvm/daemon.pid"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mvm", "server.pid")
}

// DefaultStatePath returns the state file path.
// Same path on macOS and inside Lima (shared via writable virtiofs mount).
func DefaultStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mvm", "state.json")
}

// IsLinux detects if we're running on Linux (inside Lima VM or on a cloud server).
// The daemon binary is cross-compiled for Linux.
func IsLinux() bool {
	return runtime.GOOS == "linux"
}

func New(cfg Config) (*Server, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	if cfg.PIDPath == "" {
		cfg.PIDPath = DefaultPIDPath()
	}

	if err := CheckNotRunning(cfg.PIDPath); err != nil {
		return nil, err
	}

	os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o755)
	os.Remove(cfg.SocketPath)

	ln, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.SocketPath, err)
	}
	os.Chmod(cfg.SocketPath, 0o666) // allow CLI on macOS to connect via Lima socket forward

	s := &Server{
		store:        cfg.Store,
		executor:     cfg.Executor,
		unixListener: ln,
		sockPath:     cfg.SocketPath,
		pidPath:      cfg.PIDPath,
		cfg:          cfg,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /vms", s.handleListVMs)
	mux.HandleFunc("POST /vms", s.handleCreateVM)
	mux.HandleFunc("POST /vms/{name}/exec", s.handleExec)
	mux.HandleFunc("DELETE /vms/{name}", s.handleDeleteVM)
	mux.HandleFunc("POST /vms/{name}/stop", s.handleStopVM)
	mux.HandleFunc("POST /vms/{name}/pause", s.handlePauseVM)
	mux.HandleFunc("POST /vms/{name}/resume", s.handleResumeVM)
	mux.HandleFunc("POST /vms/{name}/snapshot", s.handleSnapshotCreate)
	mux.HandleFunc("POST /vms/{name}/restore", s.handleSnapshotRestore)
	mux.HandleFunc("GET /snapshots", s.handleSnapshotList)
	mux.HandleFunc("DELETE /snapshots/{name}", s.handleSnapshotDelete)
	mux.HandleFunc("GET /pool", s.handlePoolStatus)
	mux.HandleFunc("POST /pool/warm", s.handlePoolWarm)
	mux.HandleFunc("POST /build", s.handleBuild)
	mux.HandleFunc("GET /images", s.handleImageList)
	mux.HandleFunc("DELETE /images/{name}", s.handleImageDelete)

	s.unixServer = &http.Server{Handler: mux}

	// Set up TCP listener if ListenAddr is configured.
	if cfg.ListenAddr != "" {
		tcpLn, err := net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
		}
		s.tcpListener = tcpLn

		var tcpHandler http.Handler = mux
		if cfg.APIKey != "" {
			tcpHandler = authMiddleware(cfg.APIKey, mux)
		}

		s.tcpServer = &http.Server{
			Handler:      tcpHandler,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 5 * time.Minute,
			IdleTimeout:  120 * time.Second,
		}
	}

	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	if err := WritePID(s.pidPath); err != nil {
		return fmt.Errorf("write PID file: %w", err)
	}

	log.Printf("mvm daemon listening on %s (PID %d)", s.sockPath, os.Getpid())

	g, ctx := errgroup.WithContext(ctx)

	// Context cancellation triggers graceful shutdown.
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	})

	// Unix socket server.
	g.Go(func() error {
		err := s.unixServer.Serve(s.unixListener)
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	})

	// TCP server (if configured).
	if s.tcpServer != nil && s.tcpListener != nil {
		hasTLS := s.cfg.TLSCert != "" && s.cfg.TLSKey != ""
		insecure := os.Getenv("MVM_INSECURE") == "true"

		g.Go(func() error {
			var err error
			if hasTLS && !insecure {
				log.Printf("mvm daemon TCP+TLS listening on %s", s.cfg.ListenAddr)
				cert, loadErr := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
				if loadErr != nil {
					return fmt.Errorf("load TLS cert/key: %w", loadErr)
				}
				tlsLn := tls.NewListener(s.tcpListener, &tls.Config{
					Certificates: []tls.Certificate{cert},
				})
				err = s.tcpServer.Serve(tlsLn)
			} else {
				if insecure {
					log.Printf("mvm daemon TCP (insecure) listening on %s", s.cfg.ListenAddr)
				} else {
					log.Printf("mvm daemon TCP listening on %s (no TLS configured)", s.cfg.ListenAddr)
				}
				err = s.tcpServer.Serve(s.tcpListener)
			}
			if err == http.ErrServerClosed {
				return nil
			}
			return err
		})
	}

	return g.Wait()
}

func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("mvm daemon shutting down...")
	s.unixServer.Shutdown(ctx)
	if s.tcpServer != nil {
		s.tcpServer.Shutdown(ctx)
	}
	os.Remove(s.sockPath)
	RemovePID(s.pidPath)
	return nil
}
