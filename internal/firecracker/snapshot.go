package firecracker

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
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
// Returns (pid, socketPath, error).
func RestoreVMSnapshot(exec Executor, vmName, snapDir string, alloc state.NetAllocation) (int, string, error) {
	// Decrypt if encrypted
	for _, f := range []string{"snapshot.bin", "mem.bin", "rootfs.ext4"} {
		encPath := filepath.Join(snapDir, f+".enc")
		if _, err := os.Stat(encPath); err == nil {
			key := os.Getenv(SnapshotKeyEnvVar)
			if key == "" {
				return 0, "", fmt.Errorf("snapshot is encrypted — set %s", SnapshotKeyEnvVar)
			}
			if err := decryptFile(encPath, filepath.Join(snapDir, f), key); err != nil {
				return 0, "", fmt.Errorf("decrypt %s: %w", f, err)
			}
		}
	}

	vmDir := VMDir(vmName)

	// Copy snapshot files from snapDir to VM directory
	setupCmd := fmt.Sprintf(
		`sudo mkdir -p %s && sudo cp --sparse=always %s/rootfs.ext4 %s/rootfs.ext4 && sudo cp --sparse=always %s/snapshot.bin %s/snapshot.bin && sudo cp --sparse=always %s/mem.bin %s/mem.bin && echo COPY_OK`,
		vmDir,
		snapDir, vmDir,
		snapDir, vmDir,
		snapDir, vmDir,
	)
	out, err := exec.RunWithTimeout(setupCmd, LongTimeout)
	if err != nil || !strings.Contains(out, "COPY_OK") {
		return 0, "", fmt.Errorf("copy snapshot files: %w (output: %s)", err, out)
	}

	// Start Firecracker and load the snapshot
	pid, socketPath, err := loadSnapshot(exec, vmName, alloc)
	if err != nil {
		return 0, "", err
	}

	return pid, socketPath, nil
}

// loadSnapshot starts a new Firecracker process with API socket,
// sets up TAP networking, and loads a snapshot from the VM directory.
// Modeled on pool.go's restoreSlotFromSnapshot.
func loadSnapshot(exec Executor, vmName string, alloc state.NetAllocation) (int, string, error) {
	vmDir := VMDir(vmName)
	socketPath := SocketPath(vmName)

	// Set up TAP device and log file
	setupCmd := fmt.Sprintf(
		`sudo ip link del %s 2>/dev/null; sudo ip tuntap add dev %s mode tap && sudo ip addr add %s/30 dev %s && sudo ip link set dev %s up && sudo touch %s/firecracker.log && sudo chmod 666 %s/firecracker.log && echo SETUP_OK`,
		alloc.TAPDev, alloc.TAPDev, alloc.TAPIP, alloc.TAPDev, alloc.TAPDev,
		vmDir, vmDir,
	)
	out, err := exec.Run(setupCmd)
	if err != nil || !strings.Contains(out, "SETUP_OK") {
		return 0, "", fmt.Errorf("TAP setup failed: %w (output: %s)", err, out)
	}

	// Start Firecracker with API socket (no --config-file, snapshot restore mode)
	startCmd := fmt.Sprintf(
		`sudo rm -f %s %s/%s.vsock %s/%s.vsock_5123; sudo mkdir -p %s; sudo setsid firecracker --api-sock %s </dev/null >%s/firecracker.log 2>&1 & echo $! | sudo tee %s/pid`,
		socketPath, RunDir(), vmName, RunDir(), vmName,
		RunDir(),
		socketPath,
		vmDir, vmDir,
	)
	out, err = exec.Run(startCmd)
	if err != nil {
		return 0, "", fmt.Errorf("firecracker start failed: %w", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(out))

	// Wait for API socket
	waitCmd := fmt.Sprintf(
		`for j in $(seq 1 30); do sudo test -S %s && break; sleep 0.1; done; sudo test -S %s && echo SOCK_OK`,
		socketPath, socketPath,
	)
	out, err = exec.Run(waitCmd)
	if err != nil || !strings.Contains(out, "SOCK_OK") {
		return 0, "", fmt.Errorf("API socket not ready")
	}

	// Load snapshot with network_overrides to remap TAP device
	restoreCmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/load" `+
			`-H "Content-Type: application/json" `+
			`-d '{"snapshot_path":"%s/snapshot.bin","mem_backend":{"backend_path":"%s/mem.bin","backend_type":"File"},"enable_diff_snapshots":false,"resume_vm":true,"network_overrides":[{"iface_id":"net1","host_dev_name":"%s"}]}' && echo RESTORE_OK`,
		socketPath, vmDir, vmDir, alloc.TAPDev,
	)
	out, err = exec.RunWithTimeout(restoreCmd, 30*time.Second)
	if err != nil || !strings.Contains(out, "RESTORE_OK") {
		return 0, "", fmt.Errorf("snapshot restore failed: %v (output: %s)", err, out)
	}

	return pid, socketPath, nil
}

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
