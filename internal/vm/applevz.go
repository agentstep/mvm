package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// AppleVZBackend manages VMs using Apple Virtualization.framework via the mvm-vz helper.
// No Lima required. Works on M1/M2/M3+. No pause/resume (VZ limitation).
type AppleVZBackend struct {
	binary   string // path to mvm-vz binary
	dataDir  string // ~/.mvm/
	cacheDir string // ~/.mvm/cache/
}

// NewAppleVZBackend creates a new Apple VZ backend.
func NewAppleVZBackend(mvmDir string) *AppleVZBackend {
	// Look for mvm-vz next to the mvm binary, or in PATH
	binary := "mvm-vz"
	if execPath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(execPath), "mvm-vz")
		if _, err := os.Stat(candidate); err == nil {
			binary = candidate
		}
	}

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

type vzCreateResult struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	PID      int    `json:"pid"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
}

// StartVM boots a new VM via mvm-vz create --foreground (runs in background via Go).
// Returns the PID of the mvm-vz process.
func (b *AppleVZBackend) StartVM(name, kernelPath, rootfsPath, bootArgs, mac string, cpus, memoryMB int, volumes []string) (int, error) {
	logPath := filepath.Join(b.dataDir, "vms", name, "console.log")
	os.MkdirAll(filepath.Dir(logPath), 0o755)

	args := []string{
		"create",
		"--name", name,
		"--kernel", kernelPath,
		"--rootfs", rootfsPath,
		"--cpus", strconv.Itoa(cpus),
		"--memory", strconv.Itoa(memoryMB),
		"--boot-args", bootArgs,
		"--log-path", logPath,
		"--foreground",
	}
	if mac != "" {
		args = append(args, "--mac", mac)
	}
	for _, vol := range volumes {
		// vol is "hostPath:guestPath" — pass as --share hostPath:tag
		// Use the guest path as the virtiofs tag
		args = append(args, "--share", vol)
	}

	cmd := exec.Command(b.binary, args...)
	// Capture initial JSON output
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start mvm-vz: %w", err)
	}

	// The mvm-vz process runs in foreground and blocks. We started it
	// in the background via cmd.Start(). The PID is cmd.Process.Pid.
	return cmd.Process.Pid, nil
}

// StopVM sends SIGTERM to the mvm-vz process managing the VM.
func (b *AppleVZBackend) StopVM(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
	// Signal 0 tests if process exists
	return process.Signal(nil) == nil
}

// StatusVM returns the VM status as JSON.
func (b *AppleVZBackend) StatusVM(pid int) (string, error) {
	cmd := exec.Command(b.binary, "status", "--pid", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var result map[string]interface{}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", err
	}
	state, _ := result["state"].(string)
	return state, nil
}
