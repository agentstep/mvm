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
	"time"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
)

const (
	SnapshotKeyEnvVar = "MVM_SNAPSHOT_KEY" // env var for AES-256-GCM encryption key
)

// CreateDeltaSnapshot creates a snapshot capturing changes since base image.
// Uses Lima's shared filesystem (host ~/.mvm/ is mounted inside Lima) to
// avoid shell-piping large files.
func CreateDeltaSnapshot(limaClient *lima.Client, vm *state.VM, hostSnapDir string) error {
	if err := os.MkdirAll(hostSnapDir, 0o755); err != nil {
		return err
	}

	// Pause VM before snapshot
	if err := Pause(limaClient, vm); err != nil {
		return fmt.Errorf("pause before snapshot: %w", err)
	}

	// Snapshot files go to the VM directory inside Lima (accessible via shared mount)
	limaSnapDir := "/opt/mvm/vms/" + vm.Name
	snapFile := limaSnapDir + "/snapshot.bin"
	memFile := limaSnapDir + "/mem.bin"

	// Create snapshot via Firecracker API
	script := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/create" `+
			`-H "Content-Type: application/json" `+
			`-d '{"snapshot_type":"Diff","snapshot_path":"%s","mem_file_path":"%s"}'`,
		vm.SocketPath, snapFile, memFile,
	)
	out, err := limaClient.Shell(script)
	if err != nil {
		Resume(limaClient, vm)
		return fmt.Errorf("create snapshot: %w (output: %s)", err, out)
	}

	// Copy snapshot files from Lima to host via Lima's shared filesystem
	// Lima mounts the user's home directory, so we can use limactl copy
	for _, f := range []string{"snapshot.bin", "mem.bin"} {
		src := limaSnapDir + "/" + f
		dst := filepath.Join(hostSnapDir, f)
		copyCmd := fmt.Sprintf("sudo cp %s /tmp/mvm-snap-%s && sudo chmod 644 /tmp/mvm-snap-%s", src, f, f)
		limaClient.Shell(copyCmd)

		// Use limactl copy to get the file to the host
		if err := copyFromLima(limaClient, "/tmp/mvm-snap-"+f, dst); err != nil {
			Resume(limaClient, vm)
			return fmt.Errorf("copy %s to host: %w", f, err)
		}
		limaClient.Shell(fmt.Sprintf("rm -f /tmp/mvm-snap-%s", f))
	}

	// Write metadata
	meta := fmt.Sprintf(`{"vm":"%s","created":"%s","type":"delta","socket":"%s"}`,
		vm.Name, time.Now().UTC().Format(time.RFC3339), vm.SocketPath)
	os.WriteFile(filepath.Join(hostSnapDir, "meta.json"), []byte(meta), 0o644)

	// Resume VM
	Resume(limaClient, vm)

	// Encrypt if key is set
	if key := os.Getenv(SnapshotKeyEnvVar); key != "" {
		for _, f := range []string{"snapshot.bin", "mem.bin"} {
			path := filepath.Join(hostSnapDir, f)
			if err := encryptFile(path, key); err != nil {
				return fmt.Errorf("encrypt %s: %w", f, err)
			}
		}
		fmt.Println("  Snapshot encrypted with AES-256-GCM")
	}

	return nil
}

// RestoreDeltaSnapshot restores a VM from a delta snapshot.
func RestoreDeltaSnapshot(limaClient *lima.Client, vm *state.VM, hostSnapDir string) error {
	// Decrypt if encrypted
	for _, f := range []string{"snapshot.bin", "mem.bin"} {
		encPath := filepath.Join(hostSnapDir, f+".enc")
		if _, err := os.Stat(encPath); err == nil {
			key := os.Getenv(SnapshotKeyEnvVar)
			if key == "" {
				return fmt.Errorf("snapshot is encrypted — set %s", SnapshotKeyEnvVar)
			}
			if err := decryptFile(encPath, filepath.Join(hostSnapDir, f), key); err != nil {
				return fmt.Errorf("decrypt %s: %w", f, err)
			}
		}
	}

	// Copy files from host to Lima VM
	limaDir := "/opt/mvm/vms/" + vm.Name
	limaClient.Shell(fmt.Sprintf("sudo mkdir -p %s", limaDir))

	for _, f := range []string{"snapshot.bin", "mem.bin"} {
		src := filepath.Join(hostSnapDir, f)
		if err := copyToLima(limaClient, src, "/tmp/mvm-snap-"+f); err != nil {
			return fmt.Errorf("copy %s to Lima: %w", f, err)
		}
		limaClient.Shell(fmt.Sprintf("sudo mv /tmp/mvm-snap-%s %s/%s", f, limaDir, f))
	}

	// Load snapshot via Firecracker API (network interface already configured by StartFromSnapshotScript)
	script := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/load" `+
			`-H "Content-Type: application/json" `+
			`-d '{"snapshot_path":"%s/snapshot.bin","mem_backend":{"backend_path":"%s/mem.bin","backend_type":"File"},"enable_diff_snapshots":true,"resume_vm":true}'`,
		vm.SocketPath, limaDir, limaDir,
	)
	_, err := limaClient.Shell(script)
	return err
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

// copyFromLima copies a file from the Lima VM to the host using limactl cp.
func copyFromLima(limaClient *lima.Client, limaPath, hostPath string) error {
	cmd := fmt.Sprintf("limactl copy %s:%s %s", limaClient.VMName, limaPath, hostPath)
	_, err := execLimaCtl(cmd)
	return err
}

// copyToLima copies a file from the host to the Lima VM using limactl cp.
func copyToLima(limaClient *lima.Client, hostPath, limaPath string) error {
	cmd := fmt.Sprintf("limactl copy %s %s:%s", hostPath, limaClient.VMName, limaPath)
	_, err := execLimaCtl(cmd)
	return err
}

func execLimaCtl(cmd string) (string, error) {
	return lima.ExecCmd(lima.DefaultTimeout, "bash", "-c", cmd)
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
