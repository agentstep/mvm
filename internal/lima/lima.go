package lima

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultTimeout = 30 * time.Second
	LongTimeout    = 15 * time.Minute
)

// Client wraps limactl to manage a Lima VM for Firecracker.
type Client struct {
	VMName        string
	cachedRunning *bool // cached result of EnsureRunning
}

func NewClient(vmName string) *Client {
	return &Client{VMName: vmName}
}

// IsInstalled checks if limactl is available on PATH.
func (c *Client) IsInstalled() bool {
	_, err := exec.LookPath("limactl")
	return err == nil
}

// IsBrewInstalled checks if Homebrew is available on PATH.
func (c *Client) IsBrewInstalled() bool {
	_, err := exec.LookPath("brew")
	return err == nil
}

// InstallLima installs Lima via Homebrew.
func (c *Client) InstallLima() error {
	_, err := execCmd(LongTimeout, "brew", "install", "lima")
	if err != nil {
		return fmt.Errorf("failed to install Lima: %w", err)
	}
	return nil
}

// CheckHardware verifies Apple Silicon M3+ and macOS 15+.
func (c *Client) CheckHardware() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("mvm requires macOS (detected: %s)", runtime.GOOS)
	}
	if runtime.GOARCH != "arm64" {
		return fmt.Errorf("mvm requires Apple Silicon (detected: %s)", runtime.GOARCH)
	}

	// Check chip generation: M3+ supports nested virtualization
	out, err := execCmd(DefaultTimeout, "sysctl", "-n", "machdep.cpu.brand_string")
	if err != nil {
		return fmt.Errorf("failed to detect CPU: %w", err)
	}
	chip := strings.TrimSpace(out)
	if !isM3OrNewer(chip) {
		return fmt.Errorf("mvm requires Apple Silicon M3 or newer for nested virtualization (detected: %s)", chip)
	}

	// Check macOS version: 15+ (Sequoia) required
	out, err = execCmd(DefaultTimeout, "sw_vers", "-productVersion")
	if err != nil {
		return fmt.Errorf("failed to detect macOS version: %w", err)
	}
	ver := strings.TrimSpace(out)
	major, err := parseMajorVersion(ver)
	if err != nil {
		return fmt.Errorf("failed to parse macOS version %q: %w", ver, err)
	}
	if major < 15 {
		return fmt.Errorf("mvm requires macOS 15 (Sequoia) or newer (detected: %s)", ver)
	}

	return nil
}

// isM3OrNewer checks if the CPU brand string indicates M3, M4, or newer.
func isM3OrNewer(brand string) bool {
	brand = strings.ToLower(brand)
	if !strings.Contains(brand, "apple") {
		return false
	}
	// Look for M followed by a digit >= 3
	for i, ch := range brand {
		if ch == 'm' && i+1 < len(brand) {
			next := brand[i+1]
			if next >= '3' && next <= '9' {
				return true
			}
		}
	}
	return false
}

func parseMajorVersion(ver string) (int, error) {
	parts := strings.Split(ver, ".")
	if len(parts) == 0 {
		return 0, fmt.Errorf("empty version")
	}
	return strconv.Atoi(parts[0])
}

type limaInstance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// VMExists checks if the Lima VM exists.
func (c *Client) VMExists() (bool, error) {
	out, err := execCmd(DefaultTimeout, "limactl", "list", "--json")
	if err != nil {
		return false, fmt.Errorf("limactl list failed: %w", err)
	}
	// limactl list --json outputs one JSON object per line
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var inst limaInstance
		if err := json.Unmarshal([]byte(line), &inst); err != nil {
			continue
		}
		if inst.Name == c.VMName {
			return true, nil
		}
	}
	return false, nil
}

// VMStatus returns the status of the Lima VM ("Running", "Stopped", etc.).
func (c *Client) VMStatus() (string, error) {
	out, err := execCmd(DefaultTimeout, "limactl", "list", "--json")
	if err != nil {
		return "", fmt.Errorf("limactl list failed: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var inst limaInstance
		if err := json.Unmarshal([]byte(line), &inst); err != nil {
			continue
		}
		if inst.Name == c.VMName {
			return inst.Status, nil
		}
	}
	return "", fmt.Errorf("Lima VM %q not found", c.VMName)
}

// CreateVM creates a new Lima VM with nested virtualization.
func (c *Client) CreateVM(cpus int, memory string) error {
	_, err := execCmd(LongTimeout, "limactl", "create",
		"--name="+c.VMName,
		"--containerd=none",
		fmt.Sprintf("--set=.cpus=%d | .memory=%q | .nestedVirtualization=true | .vmType=%q | .mounts=[{\"location\":\"~\",\"writable\":true}] | .portForwards=[{\"guestSocket\":\"/run/mvm/daemon.sock\",\"hostSocket\":\"{{.Dir}}/sock/daemon.sock\"}]", cpus, memory, "vz"),
		"template://default",
	)
	if err != nil {
		return fmt.Errorf("failed to create Lima VM: %w", err)
	}
	return nil
}

// StartVM starts the Lima VM.
// Tolerates limactl start errors if the VM ends up running (e.g., optional
// requirement timeouts like containerd checks).
func (c *Client) StartVM() error {
	_, err := execCmd(LongTimeout, "limactl", "start", c.VMName)
	if err != nil {
		// Check if VM is actually running despite the error
		status, statusErr := c.VMStatus()
		if statusErr == nil && status == "Running" {
			return nil // VM is running, ignore the error
		}
		return fmt.Errorf("failed to start Lima VM: %w", err)
	}
	return nil
}

// StopVM stops the Lima VM.
func (c *Client) StopVM() error {
	_, err := execCmd(LongTimeout, "limactl", "stop", c.VMName)
	if err != nil {
		return fmt.Errorf("failed to stop Lima VM: %w", err)
	}
	return nil
}

// DeleteVM deletes the Lima VM.
func (c *Client) DeleteVM() error {
	_, err := execCmd(LongTimeout, "limactl", "delete", "--force", c.VMName)
	if err != nil {
		return fmt.Errorf("failed to delete Lima VM: %w", err)
	}
	return nil
}

// EnsureRunning checks if the Lima VM is running and starts it if not.
// Result is cached for the lifetime of this Client instance to avoid
// redundant limactl calls within a single command.
func (c *Client) EnsureRunning() error {
	if c.cachedRunning != nil && *c.cachedRunning {
		return nil
	}

	exists, err := c.VMExists()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Lima VM %q does not exist. Run: mvm init", c.VMName)
	}

	status, err := c.VMStatus()
	if err != nil {
		return err
	}
	if status == "Running" {
		t := true
		c.cachedRunning = &t
		return nil
	}

	fmt.Printf("  Lima VM is %s, starting...\n", strings.ToLower(status))
	if err := c.StartVM(); err != nil {
		return err
	}
	t := true
	c.cachedRunning = &t
	return nil
}

// Run implements runner.Executor — runs a command inside the Lima VM.
func (c *Client) Run(command string) (string, error) {
	return c.ShellWithTimeout(command, DefaultTimeout)
}

// RunWithTimeout implements runner.Executor — runs a command with a timeout.
func (c *Client) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	return c.ShellWithTimeout(command, timeout)
}

// Shell runs a command inside the Lima VM and returns its stdout.
func (c *Client) Shell(command string) (string, error) {
	return c.ShellWithTimeout(command, DefaultTimeout)
}

// ShellWithTimeout runs a command inside Lima with a custom timeout.
func (c *Client) ShellWithTimeout(command string, timeout time.Duration) (string, error) {
	out, err := execCmd(timeout, "limactl", "shell", c.VMName, "bash", "-c", command)
	if err != nil {
		return "", fmt.Errorf("lima shell failed: %w\ncommand: %s\noutput: %s", err, command, out)
	}
	return out, nil
}



// ShellScript writes a script to the Lima VM and executes it as a single invocation.
// This avoids multiple SSH round-trips for multi-step operations.
func (c *Client) ShellScript(script string) (string, error) {
	return c.ShellScriptWithTimeout(script, LongTimeout)
}

// ShellScriptWithTimeout writes a script to a temp file, copies it into the Lima VM,
// and executes it. This avoids passing the script through bash -c as an argument,
// which breaks when scripts contain heredocs or nested quoting.
func (c *Client) ShellScriptWithTimeout(script string, timeout time.Duration) (string, error) {
	// Write to host temp file (no shell interpretation)
	tmp, err := os.CreateTemp("", "mvm-script-*.sh")
	if err != nil {
		return "", fmt.Errorf("create temp script: %w", err)
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString("#!/bin/bash\nset -e\n")
	tmp.WriteString(script)
	tmp.Close()

	// Copy to Lima via limactl copy (scp, no shell parsing)
	remotePath := "/tmp/mvm-script-" + filepath.Base(tmp.Name())
	copyCmd := exec.Command("limactl", "copy", tmp.Name(), c.VMName+":"+remotePath)
	if out, err := copyCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("copy script to Lima: %w (%s)", err, string(out))
	}

	// Execute as a file (not as bash -c argument)
	defer func() {
		execCmd(5*time.Second, "limactl", "shell", c.VMName, "rm", "-f", remotePath)
	}()
	return execCmd(timeout, "limactl", "shell", c.VMName, "bash", remotePath)
}

// ShellInteractive runs a command inside Lima with stdin/stdout/stderr attached.
// Used for SSH sessions.
func (c *Client) ShellInteractive(command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "limactl", "shell", c.VMName, "bash", "-c", command)
	cmd.Stdin = nil // will be set by caller
	cmd.Stdout = nil
	cmd.Stderr = nil

	// For interactive use, we need the raw exec
	cmd.Stdin = execStdin()
	cmd.Stdout = execStdout()
	cmd.Stderr = execStderr()

	return cmd.Run()
}

// ExecCmd runs a command with a timeout and returns stdout.
func ExecCmd(timeout time.Duration, name string, args ...string) (string, error) {
	return execCmd(timeout, name, args...)
}

func execCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// WaitDelay: after the context kills the process, wait this long then
	// forcibly close I/O pipes. Without this, cmd.Wait() hangs forever when
	// grandchild processes (e.g., SSH spawned by limactl) keep stdout open
	// after the parent is killed. This was the root cause of the blocking
	// mvm start hang on cold boot.
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), fmt.Errorf("command timed out after %v: %s %s", timeout, name, strings.Join(args, " "))
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}
