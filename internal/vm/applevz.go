package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/vzhelper"
)

// AppleVZBackend manages VMs using Apple Virtualization.framework via the
// mvm-vz Swift helper. No Lima required. Works on M1/M2/M3+.
//
// As of PR #2, each running mvm-vz process listens on a per-VM Unix
// socket at ~/.mvm/run/vz-<name>.sock. The Go side uses that socket to
// open vsock connections into the in-guest agent (via SCM_RIGHTS) and
// to drive pause/resume/stop.
type AppleVZBackend struct {
	binary   string // path to mvm-vz binary
	dataDir  string // ~/.mvm/
	cacheDir string // ~/.mvm/cache/
}

// NewAppleVZBackend creates a new Apple VZ backend.
func NewAppleVZBackend(mvmDir string) *AppleVZBackend {
	binary := vzhelper.HelperBinary()
	return &AppleVZBackend{
		binary:   binary,
		dataDir:  mvmDir,
		cacheDir: filepath.Join(mvmDir, "cache"),
	}
}

// Name returns the backend identifier.
func (b *AppleVZBackend) Name() string { return "applevz" }

// IsAvailable checks if the mvm-vz binary exists and Virtualization.framework works.
func (b *AppleVZBackend) IsAvailable() bool {
	cmd := exec.Command(b.binary, "version")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Virtualization.framework")
}

// IPCSocketPath returns the per-VM helper IPC socket path for a given VM name.
// This is the canonical path the Swift helper binds to and the Go side dials.
func (b *AppleVZBackend) IPCSocketPath(name string) string {
	return vzhelper.SocketPath(b.dataDir, name)
}

// AgentClient returns an agent client targeting the in-guest agent via the
// per-VM mvm-vz helper. The client opens a fresh vsock fd per request and
// closes it after — see internal/agentclient for the contract.
func (b *AppleVZBackend) AgentClient(name string) *agentclient.Client {
	return agentclient.New(&agentclient.VZSocketDialer{
		SocketPath: b.IPCSocketPath(name),
	})
}

// HelperClient returns a vzhelper client for VM-lifecycle operations
// (pause/resume/stop/status). Distinct from AgentClient: this one talks
// to the helper itself, not through it to the in-guest agent.
func (b *AppleVZBackend) HelperClient(name string) *vzhelper.Client {
	return vzhelper.New(b.IPCSocketPath(name))
}

// vzCreateResult is the JSON status line the mvm-vz helper prints
// immediately after starting a VM and binding its IPC socket.
type vzCreateResult struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	PID       int    `json:"pid"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memory_mb"`
	IPCSocket string `json:"ipc_socket"`
}

// StartResult is returned by StartVM with the running helper's PID and
// the IPC socket it's listening on.
type StartResult struct {
	PID       int
	IPCSocket string
}

// StartVM boots a new VM via mvm-vz create --foreground.
//
// The mvm-vz process runs in the background (detached from the caller's
// terminal but still a child of the Go process). After it has started
// the VM and bound the IPC socket, it prints a single JSON status line
// to stdout — this method reads that line synchronously, so the returned
// StartResult is only populated once the IPC socket is ready to accept
// connections.
func (b *AppleVZBackend) StartVM(name, kernelPath, rootfsPath, bootArgs, mac string, cpus, memoryMB int, volumes []string) (*StartResult, error) {
	logPath := filepath.Join(b.dataDir, "vms", name, "console.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir vm dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(b.dataDir, "run"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir run dir: %w", err)
	}

	ipcSocket := b.IPCSocketPath(name)

	args := []string{
		"create",
		"--name", name,
		"--kernel", kernelPath,
		"--rootfs", rootfsPath,
		"--cpus", strconv.Itoa(cpus),
		"--memory", strconv.Itoa(memoryMB),
		"--boot-args", bootArgs,
		"--log-path", logPath,
		"--ipc-socket", ipcSocket,
		"--foreground",
	}
	if mac != "" {
		args = append(args, "--mac", mac)
	}
	for _, vol := range volumes {
		// Pass through; see Create.swift NOTE for the share-format caveat.
		args = append(args, "--share", vol)
	}

	cmd := exec.Command(b.binary, args...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mvm-vz: %w", err)
	}

	// Read the JSON status line the helper prints right after VM boot
	// and IPC bind. We use a goroutine + timeout so a hung helper can't
	// deadlock the caller.
	br := bufio.NewReader(stdoutPipe)
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := br.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- strings.TrimSpace(line)
	}()

	var jsonLine string
	select {
	case jsonLine = <-lineCh:
		// got it
	case err := <-errCh:
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("read mvm-vz status: %w", err)
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("timeout waiting for mvm-vz status line")
	}

	// Drain the rest of stdout in the background so the helper doesn't
	// block on a full pipe down the line.
	go func() {
		_, _ = io.Copy(io.Discard, br)
	}()

	var info vzCreateResult
	if err := json.Unmarshal([]byte(jsonLine), &info); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("parse mvm-vz status %q: %w", jsonLine, err)
	}

	// The helper-reported PID matches cmd.Process.Pid (it's
	// ProcessInfo.processIdentifier in Swift). We trust the JSON because
	// the helper prints it after IPC bind, which is the readiness signal
	// we actually care about.
	socket := info.IPCSocket
	if socket == "" {
		socket = ipcSocket
	}
	return &StartResult{PID: info.PID, IPCSocket: socket}, nil
}

// StopVM asks the helper for a graceful shutdown via the IPC socket.
// Falls back to SIGTERM-via-mvm-vz-stop if the IPC socket is unreachable
// (helper alive but IPC broken — shouldn't normally happen).
func (b *AppleVZBackend) StopVM(name string, pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Preferred: graceful stop via IPC.
	helper := b.HelperClient(name)
	if err := helper.Stop(ctx); err == nil {
		return nil
	}

	// Fallback: send SIGTERM via the legacy stop subcommand.
	cmd := exec.CommandContext(ctx, b.binary, "stop", "--pid", strconv.Itoa(pid))
	return cmd.Run()
}

// IsRunning checks if the mvm-vz process is alive.
func (b *AppleVZBackend) IsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests if process exists.
	return process.Signal(nil) == nil
}

// StatusVM returns the VM status by querying the helper IPC.
// Falls back to "unknown" if the helper isn't reachable.
func (b *AppleVZBackend) StatusVM(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	helper := b.HelperClient(name)
	state, err := helper.Status(ctx)
	if err != nil {
		return "unknown", err
	}
	return state, nil
}
