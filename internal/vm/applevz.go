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
	"syscall"
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
func (b *AppleVZBackend) StartVM(name, kernelPath, rootfsPath, bootArgs, mac string, cpus, memoryMB int, volumes []string, restoreFrom string) (*StartResult, error) {
	logPath := filepath.Join(b.dataDir, "vms", name, "console.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir vm dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(b.dataDir, "run"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir run dir: %w", err)
	}

	// The helper's own diagnostic stderr (distinct from --log-path, which
	// captures the guest's serial console) — see the cmd.Stderr comment
	// below for why this goes to a file rather than os.Stderr.
	stderrLogPath := filepath.Join(filepath.Dir(logPath), "mvm-vz-stderr.log")

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
	// Always build a save/restore-compatible config (omits entropy/console/
	// balloon) so that any VM can be snapshotted and restored — VZ's actual
	// save/restore rejects those devices even though validate() accepts them.
	args = append(args, "--save-restore")
	// Persist the machine identifier per VM so the restore config matches the
	// saved one — a fresh random identifier each run is what made restore fail
	// with VZError.restore ("invalid argument").
	args = append(args, "--machine-id-path", filepath.Join(b.dataDir, "vms", name, "machine-id"))
	if restoreFrom != "" {
		args = append(args, "--restore-from", restoreFrom)
	}
	for _, vol := range volumes {
		// Pass through; see Create.swift NOTE for the share-format caveat.
		args = append(args, "--share", vol)
	}

	cmd := exec.Command(b.binary, args...)
	// Put the helper in its own session (setsid) so a Ctrl-C / SIGINT sent
	// to the CLI's foreground process group is NOT also delivered to the VM.
	// Without this, pressing Ctrl-C during the post-start agent wait tears
	// the VM down even though state already records it as running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// mvm-vz --foreground stays alive for the VM's whole lifetime, detached
	// from (but still a fork-child of) this process. cmd.Stderr used to be
	// os.Stderr, which handed the helper a dup of *this process's own*
	// stderr fd — if that fd is itself a pipe a caller of `mvm start` is
	// reading to EOF (execFile, `$(...)`, a shell pipeline), the helper's
	// long-lived copy of it keeps the pipe writable forever, hanging the
	// caller even though `mvm start` itself has long since exited. Redirect
	// to a per-VM log file instead: a regular file has no such EOF-blocking
	// behavior, and a startup failure (the only case this stream matters
	// for — see Create.swift's `throw error`/fputs paths) is still fully
	// captured for helperStderrTail to surface below.
	stderrFile, err := os.Create(stderrLogPath)
	if err != nil {
		return nil, fmt.Errorf("create helper stderr log: %w", err)
	}
	cmd.Stderr = stderrFile

	if err := cmd.Start(); err != nil {
		stderrFile.Close()
		return nil, fmt.Errorf("start mvm-vz: %w", err)
	}
	// The child has its own dup of the fd from the fork/exec; our copy of
	// the *os.File is no longer needed once cmd.Start() returns.
	stderrFile.Close()

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
		return nil, withHelperStderr(fmt.Errorf("read mvm-vz status: %w", err), stderrLogPath)
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, withHelperStderr(fmt.Errorf("timeout waiting for mvm-vz status line"), stderrLogPath)
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
		return nil, withHelperStderr(fmt.Errorf("parse mvm-vz status %q: %w", jsonLine, err), stderrLogPath)
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

// maxHelperStderrTail bounds how much of the helper's captured stderr log
// gets folded into a startup-failure error message.
const maxHelperStderrTail = 4096

// helperStderrTail returns the tail of the helper's captured stderr log
// (see the cmd.Stderr comment in StartVM for why it's a file rather than an
// inherited fd), trimmed to maxHelperStderrTail bytes. Best-effort: returns
// "" on any read error, e.g. if the file was never created.
func helperStderrTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if len(s) > maxHelperStderrTail {
		s = s[len(s)-maxHelperStderrTail:]
	}
	return s
}

// withHelperStderr enriches a startup-failure error with the helper's
// captured stderr output, if any was written — this is how error-surfacing
// is preserved now that the helper's stderr no longer goes directly to this
// process's own os.Stderr (see StartVM).
func withHelperStderr(base error, stderrLogPath string) error {
	if tail := helperStderrTail(stderrLogPath); tail != "" {
		return fmt.Errorf("%w (mvm-vz stderr: %s)", base, tail)
	}
	return base
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
		b.waitForExit(pid, 5*time.Second)
		return nil
	}

	// Fallback: send SIGTERM via the legacy stop subcommand.
	cmd := exec.CommandContext(ctx, b.binary, "stop", "--pid", strconv.Itoa(pid))
	err := cmd.Run()
	b.waitForExit(pid, 5*time.Second)
	return err
}

// waitForExit blocks until the helper process exits (releasing its exclusive
// lock on the disk image) or the timeout elapses. Without this, a restore that
// immediately follows a stop can fail to attach the disk ("storage device
// attachment is invalid") because the previous helper still holds it.
func (b *AppleVZBackend) waitForExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !b.IsRunning(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	// kill(pid, 0) probes for existence without delivering a signal.
	// It must be syscall.Signal(0): os.Process.Signal(nil) fails the
	// internal syscall.Signal type assertion and always returns an error,
	// which made this method report every process — alive or dead — as
	// not running.
	return process.Signal(syscall.Signal(0)) == nil
}

// SaveVM pauses the VM and writes its full memory+CPU+device state to
// statePath via the helper. The VM remains paused; the caller typically stops
// it next. Save writes all of guest memory to disk, so the timeout is generous.
func (b *AppleVZBackend) SaveVM(name, statePath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return b.HelperClient(name).Save(ctx, statePath)
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
