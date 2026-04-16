package firecracker

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

const (
	SnapshotKeyEnvVar = "MVM_SNAPSHOT_KEY" // env var for AES-256-GCM encryption key
)

// SnapshotVM creates a Full snapshot of a running VM.
// The snapshot files (snapshot.bin, mem.bin, rootfs.ext4) are written to snapDir.
// The VM is paused during the snapshot and resumed afterwards.
func SnapshotVM(exec Executor, vm *state.VM, snapDir string) error {
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}

	// Pause VM before snapshot
	if err := Pause(exec, vm); err != nil {
		return fmt.Errorf("pause before snapshot: %w", err)
	}

	// Snapshot files are written to the VM directory (Firecracker writes them there)
	vmDir := VMDir(vm.Name)
	snapFile := vmDir + "/snapshot.bin"
	memFile := vmDir + "/mem.bin"

	// Create snapshot via Firecracker API (Full type)
	cmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/create" `+
			`-H "Content-Type: application/json" `+
			`-d '{"snapshot_type":"Full","snapshot_path":"%s","mem_file_path":"%s"}'`,
		vm.SocketPath, snapFile, memFile,
	)
	out, err := exec.Run(cmd)
	if err != nil {
		Resume(exec, vm)
		return fmt.Errorf("create snapshot: %w (output: %s)", err, out)
	}

	// Copy snapshot files from VM directory to snapDir
	for _, f := range []string{"snapshot.bin", "mem.bin"} {
		src := vmDir + "/" + f
		dst := filepath.Join(snapDir, f)
		cpCmd := fmt.Sprintf("sudo cp --sparse=always %s %s", src, dst)
		if _, err := exec.Run(cpCmd); err != nil {
			Resume(exec, vm)
			return fmt.Errorf("copy %s to snapshot dir: %w", f, err)
		}
	}

	// Copy rootfs.ext4 into the snapshot dir (required for restore)
	rootfsSrc := vmDir + "/rootfs.ext4"
	rootfsDst := filepath.Join(snapDir, "rootfs.ext4")
	cpCmd := fmt.Sprintf("sudo cp --sparse=always %s %s", rootfsSrc, rootfsDst)
	if _, err := exec.Run(cpCmd); err != nil {
		Resume(exec, vm)
		return fmt.Errorf("copy rootfs to snapshot dir: %w", err)
	}

	// Write metadata
	meta := fmt.Sprintf(`{"vm":"%s","created":"%s","type":"full","socket":"%s"}`,
		vm.Name, time.Now().UTC().Format(time.RFC3339), vm.SocketPath)
	os.WriteFile(filepath.Join(snapDir, "meta.json"), []byte(meta), 0o644)

	// Resume VM
	Resume(exec, vm)

	// Encrypt if key is set
	if key := os.Getenv(SnapshotKeyEnvVar); key != "" {
		for _, f := range []string{"snapshot.bin", "mem.bin", "rootfs.ext4"} {
			path := filepath.Join(snapDir, f)
			if err := encryptFile(path, key); err != nil {
				return fmt.Errorf("encrypt %s: %w", f, err)
			}
		}
		fmt.Println("  Snapshot encrypted with AES-256-GCM")
	}

	return nil
}

// RestoreVMSnapshot restores a VM from a Full snapshot.
// It copies snapshot files into the VM directory, starts a new Firecracker
// process, and loads the snapshot via the API.
//
// Returns (pid, socketPath, uffdPid, error). uffdPid is the PID of the
// mvm-uffd sidecar that services guest page faults on demand; it is 0 when
// the File-backend fallback path is used (e.g. MVM_NO_UFFD=1 or handler
// startup failed).
func RestoreVMSnapshot(exec Executor, vmName, snapDir string, alloc state.NetAllocation) (int, string, int, error) {
	// Decrypt if encrypted
	for _, f := range []string{"snapshot.bin", "mem.bin", "rootfs.ext4"} {
		encPath := filepath.Join(snapDir, f+".enc")
		if _, err := os.Stat(encPath); err == nil {
			key := os.Getenv(SnapshotKeyEnvVar)
			if key == "" {
				return 0, "", 0, fmt.Errorf("snapshot is encrypted — set %s", SnapshotKeyEnvVar)
			}
			if err := decryptFile(encPath, filepath.Join(snapDir, f), key); err != nil {
				return 0, "", 0, fmt.Errorf("decrypt %s: %w", f, err)
			}
		}
	}

	vmDir := VMDir(vmName)

	// Copy rootfs (guest writes to it — COW with reflinks would be ideal
	// but we use sparse copy for portability). Snapshot.bin is tiny. With
	// UFFD, mem.bin stays in snapDir and is mmap'd by the handler directly
	// — saves a multi-GB copy. Without UFFD, we have to copy mem.bin into
	// vmDir because the File backend wants the file at that path.
	useUFFD := os.Getenv("MVM_NO_UFFD") == ""
	var setupCmd string
	if useUFFD {
		setupCmd = fmt.Sprintf(
			`sudo mkdir -p %s && sudo cp --sparse=always %s/rootfs.ext4 %s/rootfs.ext4 && sudo cp --sparse=always %s/snapshot.bin %s/snapshot.bin && echo COPY_OK`,
			vmDir, snapDir, vmDir, snapDir, vmDir,
		)
	} else {
		setupCmd = fmt.Sprintf(
			`sudo mkdir -p %s && sudo cp --sparse=always %s/rootfs.ext4 %s/rootfs.ext4 && sudo cp --sparse=always %s/snapshot.bin %s/snapshot.bin && sudo cp --sparse=always %s/mem.bin %s/mem.bin && echo COPY_OK`,
			vmDir,
			snapDir, vmDir,
			snapDir, vmDir,
			snapDir, vmDir,
		)
	}
	out, err := exec.RunWithTimeout(setupCmd, LongTimeout)
	if err != nil || !strings.Contains(out, "COPY_OK") {
		return 0, "", 0, fmt.Errorf("copy snapshot files: %w (output: %s)", err, out)
	}

	// Start Firecracker and load the snapshot. When UFFD is enabled, the
	// handler mmaps snapDir/mem.bin directly (no copy needed). Pass the
	// snapDir so loadSnapshot can find it.
	pid, socketPath, uffdPid, err := loadSnapshotWithMem(exec, vmName, alloc, snapDir)
	if err != nil {
		return 0, "", 0, err
	}

	return pid, socketPath, uffdPid, nil
}

// loadSnapshot starts a new Firecracker process with API socket,
// sets up TAP networking, and loads a snapshot from the VM directory.
// Modeled on pool.go's restoreSlotFromSnapshot.
//
// When the MVM_NO_UFFD env var is unset, loadSnapshot also spawns the
// mvm-uffd page-fault handler as a subprocess and tells Firecracker to use
// the Uffd memory backend. If the handler cannot be started (binary missing,
// socket never appears, etc.) we fall back to the File backend so the
// daemon stays functional — this is the documented rollback escape hatch.
//
// The returned uffdPid is the handler PID when UFFD is active, or 0 for
// the File-backend path.
// loadSnapshotWithMem is like loadSnapshot but takes an explicit memFileDir —
// used when UFFD is enabled so the handler can mmap mem.bin directly from the
// snapshot directory (no copy). Pass "" to fall back to vmDir/mem.bin.
func loadSnapshotWithMem(ex Executor, vmName string, alloc state.NetAllocation, memFileDir string) (pid int, socketPath string, uffdPid int, err error) {
	return loadSnapshotImpl(ex, vmName, alloc, memFileDir)
}

func loadSnapshot(ex Executor, vmName string, alloc state.NetAllocation) (pid int, socketPath string, uffdPid int, err error) {
	return loadSnapshotImpl(ex, vmName, alloc, "")
}

func loadSnapshotImpl(ex Executor, vmName string, alloc state.NetAllocation, memFileDir string) (pid int, socketPath string, uffdPid int, err error) {
	vmDir := VMDir(vmName)
	socketPath = SocketPath(vmName)

	// Set up TAP device and log file
	setupCmd := fmt.Sprintf(
		`sudo ip link del %s 2>/dev/null; sudo ip tuntap add dev %s mode tap && sudo ip addr add %s/30 dev %s && sudo ip link set dev %s up && sudo touch %s/firecracker.log && sudo chmod 666 %s/firecracker.log && echo SETUP_OK`,
		alloc.TAPDev, alloc.TAPDev, alloc.TAPIP, alloc.TAPDev, alloc.TAPDev,
		vmDir, vmDir,
	)
	out, err := ex.Run(setupCmd)
	if err != nil || !strings.Contains(out, "SETUP_OK") {
		return 0, "", 0, fmt.Errorf("TAP setup failed: %w (output: %s)", err, out)
	}

	// Start Firecracker with API socket (no --config-file, snapshot restore mode)
	startCmd := fmt.Sprintf(
		`sudo rm -f %s %s/%s.vsock %s/%s.vsock_5123; sudo mkdir -p %s; sudo setsid firecracker --api-sock %s </dev/null >%s/firecracker.log 2>&1 & echo $! | sudo tee %s/pid`,
		socketPath, RunDir(), vmName, RunDir(), vmName,
		RunDir(),
		socketPath,
		vmDir, vmDir,
	)
	out, err = ex.Run(startCmd)
	if err != nil {
		return 0, "", 0, fmt.Errorf("firecracker start failed: %w", err)
	}
	pid, _ = strconv.Atoi(strings.TrimSpace(out))

	// Wait for API socket
	waitCmd := fmt.Sprintf(
		`for j in $(seq 1 30); do sudo test -S %s && break; sleep 0.1; done; sudo test -S %s && echo SOCK_OK`,
		socketPath, socketPath,
	)
	out, err = ex.Run(waitCmd)
	if err != nil || !strings.Contains(out, "SOCK_OK") {
		return 0, "", 0, fmt.Errorf("API socket not ready")
	}

	// Decide memory backend: UFFD (default) or File (rollback / test path).
	// For UFFD, the handler can mmap the memory file from any accessible
	// location (defaults to vmDir/mem.bin; memFileDir overrides to point
	// at the snapshot dir to skip a multi-GB copy).
	memFileForUFFD := vmDir + "/mem.bin"
	if memFileDir != "" {
		memFileForUFFD = memFileDir + "/mem.bin"
	}
	backendType := "File"
	backendPath := vmDir + "/mem.bin"
	if os.Getenv("MVM_NO_UFFD") == "" {
		uffdSock := fmt.Sprintf("%s/%s-uffd.sock", RunDir(), vmName)
		if hpid, herr := startUFFDHandler(uffdSock, memFileForUFFD); herr == nil {
			uffdPid = hpid
			backendType = "Uffd"
			backendPath = uffdSock
		} else {
			log.Printf("mvm-uffd unavailable, falling back to File backend: %v", herr)
		}
	}

	// Load snapshot with network_overrides to remap TAP device
	restoreCmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/load" `+
			`-H "Content-Type: application/json" `+
			`-d '{"snapshot_path":"%s/snapshot.bin","mem_backend":{"backend_path":"%s","backend_type":"%s"},"enable_diff_snapshots":false,"resume_vm":true,"network_overrides":[{"iface_id":"net1","host_dev_name":"%s"}]}' && echo RESTORE_OK`,
		socketPath, vmDir, backendPath, backendType, alloc.TAPDev,
	)
	out, err = ex.RunWithTimeout(restoreCmd, 30*time.Second)
	if err != nil || !strings.Contains(out, "RESTORE_OK") {
		// If we started a UFFD handler, kill it so we don't leak a process.
		if uffdPid > 0 {
			_ = killUFFDHandler(uffdPid)
		}
		return 0, "", 0, fmt.Errorf("snapshot restore failed: %v (output: %s)", err, out)
	}

	return pid, socketPath, uffdPid, nil
}

// uffdBinaryCandidates lists the install paths we search for mvm-uffd when
// it is not found on $PATH. Cloud Linux installs to /usr/local/bin; the
// Lima (macOS dev) image drops it at /opt/mvm/mvm-uffd.
var uffdBinaryCandidates = []string{
	"/usr/local/bin/mvm-uffd",
	"/opt/mvm/mvm-uffd",
}

// findUFFDBinary returns an absolute path to the mvm-uffd binary, or an
// error if none of the known locations contain it.
func findUFFDBinary() (string, error) {
	if p, err := exec.LookPath("mvm-uffd"); err == nil {
		return p, nil
	}
	for _, p := range uffdBinaryCandidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("mvm-uffd binary not found (looked on $PATH and in %v)", uffdBinaryCandidates)
}

// startUFFDHandler launches mvm-uffd as a background subprocess bound to
// sockPath and backed by memFile. It returns the handler PID once the
// socket file appears (polled every 50ms for up to 2s) or an error if the
// process exits early or never binds.
//
// The handler is started directly via os/exec rather than through the
// Executor shell abstraction because we need a long-lived child process,
// its PID, and to be able to SIGKILL it on cleanup — none of which the
// Executor interface exposes.
func startUFFDHandler(sockPath, memFile string) (int, error) {
	bin, err := findUFFDBinary()
	if err != nil {
		return 0, err
	}

	// Remove any stale socket — previous run may have crashed before cleanup.
	_ = os.Remove(sockPath)

	// Make sure the run dir exists; harmless if already present.
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", filepath.Dir(sockPath), err)
	}

	cmd := exec.Command(bin,
		"--socket="+sockPath,
		"--mem="+memFile,
	)
	// Detach from the daemon's controlling terminal / stdin. Pipe the
	// handler's logs into the Firecracker log directory so operators can
	// diagnose page-fault failures.
	cmd.Stdin = nil
	if logF, ferr := os.OpenFile(
		filepath.Join(filepath.Dir(memFile), "mvm-uffd.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644,
	); ferr == nil {
		cmd.Stdout = logF
		cmd.Stderr = logF
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start mvm-uffd: %w", err)
	}

	// Release the child so it doesn't become a zombie when the daemon
	// doesn't Wait() on it. A background goroutine drains its exit.
	go func() { _ = cmd.Wait() }()

	// Poll for the socket to appear (the handler binds it on startup).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return cmd.Process.Pid, nil
		}
		// Did the child die early? Fail fast rather than waiting out the
		// full 2s budget.
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return 0, fmt.Errorf("mvm-uffd exited before binding socket (code=%d)",
				cmd.ProcessState.ExitCode())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Socket never appeared — kill the handler and report.
	_ = cmd.Process.Kill()
	return 0, fmt.Errorf("mvm-uffd socket %s did not appear within 2s", sockPath)
}

// killUFFDHandler sends SIGKILL to the mvm-uffd process identified by pid.
// It is a no-op on pid<=0 and on "no such process" errors (the handler may
// have exited cleanly when Firecracker closed its UFFD fd). Returns any
// other kill error for logging.
func killUFFDHandler(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Kill(); err != nil {
		// Treat "process already gone" as success — common and expected.
		if strings.Contains(err.Error(), "process already finished") ||
			strings.Contains(err.Error(), "no such process") {
			return nil
		}
		return err
	}
	return nil
}

// KillUFFDHandler is the exported wrapper used by callers (e.g. the stop
// path in the daemon) to reap a tracked UFFD sidecar.
func KillUFFDHandler(pid int) error { return killUFFDHandler(pid) }

// ListSnapshots returns snapshot directories under the given base path.
func ListSnapshots(baseDir string) ([]string, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var snapshots []string
	for _, e := range entries {
		if e.IsDir() {
			metaPath := filepath.Join(baseDir, e.Name(), "meta.json")
			if _, err := os.Stat(metaPath); err == nil {
				snapshots = append(snapshots, e.Name())
			}
		}
	}
	return snapshots, nil
}

// File encryption/decryption using AES-256-GCM

func encryptFile(path, hexKey string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	key := deriveKey(hexKey)
	encrypted, err := encryptAESGCM(data, key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".enc", encrypted, 0o600); err != nil {
		return err
	}
	return os.Remove(path) // remove plaintext
}

func decryptFile(encPath, plainPath, hexKey string) error {
	data, err := os.ReadFile(encPath)
	if err != nil {
		return err
	}
	key := deriveKey(hexKey)
	decrypted, err := decryptAESGCM(data, key)
	if err != nil {
		return err
	}
	return os.WriteFile(plainPath, decrypted, 0o600)
}

func deriveKey(input string) []byte {
	h := sha256.Sum256([]byte(input))
	return h[:]
}

func encryptAESGCM(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptAESGCM(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
