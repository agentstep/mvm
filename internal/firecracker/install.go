package firecracker

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/agentstep/mvm/internal/lima"
)

//go:embed scripts/build-rootfs.sh
var buildRootfsScript string

//go:embed scripts/chroot-setup.sh
var chrootSetupScript string

const (
	DefaultVersion = "v1.13.0"
	Arch           = "aarch64"
)

// Install downloads and installs Firecracker inside the Lima VM.
func Install(limaClient *lima.Client, version string) error {
	script := fmt.Sprintf(`#!/bin/bash
set -e

# Skip download if already installed with the REQUESTED version
FC_VERSION="%s"
if command -v firecracker &>/dev/null; then
    installed=$(firecracker --version 2>&1 | head -1 || true)
    if echo "$installed" | grep -q "$FC_VERSION"; then
        echo "Firecracker $FC_VERSION already installed"
        echo "Firecracker installed successfully"
        exit 0
    fi
    echo "Firecracker present ($installed) but want $FC_VERSION — upgrading..."
fi
ARCH="%s"

cd /tmp
rm -rf firecracker-install
mkdir firecracker-install && cd firecracker-install

echo "Downloading Firecracker ${FC_VERSION}..."
wget -q "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz" -O fc.tgz
tar -xzf fc.tgz

sudo mv release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH} /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker

cd /tmp && rm -rf firecracker-install

# Verify
firecracker --version
echo "Firecracker installed successfully"
`, version, Arch)

	out, err := limaClient.ShellScript(script)
	if err != nil {
		return fmt.Errorf("install Firecracker: %w", err)
	}
	fmt.Print(out)
	return nil
}

// DownloadImages downloads the kernel and builds a Debian Linux rootfs.
// Minimal busybox init (no systemd, no SSH) — boots in ~2s. Vsock agent only.
// If minimal is true, only base packages are installed (no AI agents).
func DownloadImages(limaClient *lima.Client, cacheDir string, minimal bool) error {
	minimalFlag := "0"
	if minimal {
		minimalFlag = "1"
	}

	// Write scripts to host temp files, copy to Lima via limactl copy.
	// This avoids ALL shell quoting issues — scripts never pass through bash -c.
	scripts := map[string]string{
		"/tmp/build-rootfs.sh":  buildRootfsScript,
		"/tmp/chroot-setup.sh": chrootSetupScript,
	}
	for remotePath, content := range scripts {
		tmp, err := os.CreateTemp("", "mvm-*.sh")
		if err != nil {
			return fmt.Errorf("create temp file: %w", err)
		}
		tmp.WriteString(content)
		tmp.Close()
		copyCmd := exec.Command("limactl", "copy", tmp.Name(), limaClient.VMName+":"+remotePath)
		if out, err := copyCmd.CombinedOutput(); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("copy script to Lima: %w (%s)", err, string(out))
		}
		os.Remove(tmp.Name())
		limaClient.Shell("chmod +x " + remotePath)
	}

	// Execute the build script with env vars
	env := fmt.Sprintf("CACHE_DIR=%s ARCH=%s MINIMAL=%s AGENT_BIN=/opt/mvm/mvm-agent", cacheDir, Arch, minimalFlag)
	out, err := limaClient.ShellWithTimeout(env+" /tmp/build-rootfs.sh", lima.LongTimeout*2)
	if err != nil {
		return fmt.Errorf("build rootfs: %w", err)
	}
	fmt.Print(out)
	return nil
}

// GenerateSSHKeys creates an ed25519 keypair and injects the public key into the base rootfs.
func GenerateSSHKeys(limaClient *lima.Client, keyDir, cacheDir string) error {
	script := fmt.Sprintf(`#!/bin/bash
set -e

KEY_DIR="%s"
CACHE_DIR="%s"
sudo mkdir -p "$KEY_DIR"

# Generate keypair
if [ ! -f "$KEY_DIR/mvm.id_ed25519" ]; then
    sudo ssh-keygen -t ed25519 -f "$KEY_DIR/mvm.id_ed25519" -N "" -q
    echo "SSH keypair generated"
else
    echo "SSH keypair already exists"
fi

# Inject public key into base rootfs
echo "Injecting SSH key into rootfs..."
MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$CACHE_DIR/base.ext4" "$MOUNT_DIR"
sudo mkdir -p "$MOUNT_DIR/root/.ssh"
sudo cp "$KEY_DIR/mvm.id_ed25519.pub" "$MOUNT_DIR/root/.ssh/authorized_keys"
sudo chmod 700 "$MOUNT_DIR/root/.ssh"
sudo chmod 600 "$MOUNT_DIR/root/.ssh/authorized_keys"

# Configure DNS
echo "nameserver 8.8.8.8" | sudo tee "$MOUNT_DIR/etc/resolv.conf" >/dev/null

sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
echo "SSH key injected into rootfs"
`, keyDir, cacheDir)

	out, err := limaClient.ShellScript(script)
	if err != nil {
		return fmt.Errorf("generate SSH keys: %w", err)
	}
	fmt.Print(out)
	return nil
}

// CreateSnapshot boots a template VM, waits for SSH, then creates a Firecracker snapshot.
// This snapshot is restored on every subsequent mvm start for instant boot.
func CreateSnapshot(limaClient *lima.Client, cacheDir, snapshotDir, keyDir string) error {
	script := fmt.Sprintf(`#!/bin/bash
set -e

CACHE_DIR="%s"
SNAPSHOT_DIR="%s"
KEY_DIR="%s"
SOCKET="/tmp/mvm-snapshot.socket"

echo "Creating template VM for snapshot..."
sudo mkdir -p "$SNAPSHOT_DIR"

# Copy rootfs for template
sudo cp --sparse=always "$CACHE_DIR/base.ext4" "$SNAPSHOT_DIR/rootfs.ext4"

# Create TAP for template (uses same subnet as VM index 0: tap0/172.16.0.0/30)
# This way the snapshot's guest network matches the first restored VM.
sudo ip link del tap0 2>/dev/null || true
sudo ip tuntap add dev tap0 mode tap
sudo ip addr add 172.16.0.1/30 dev tap0
sudo ip link set dev tap0 up

# Clean up any old socket
sudo rm -f "$SOCKET"

# Write config
cat > /tmp/mvm-snapshot-config.json <<'EOF'
{
  "boot-source": {
    "kernel_image_path": "%s/vmlinux",
    "boot_args": "keep_bootcon console=ttyS0 reboot=k panic=1 quiet random.trust_cpu=on rootfstype=ext4 ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": "%s/rootfs.ext4",
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "network-interfaces": [
    {
      "iface_id": "net1",
      "guest_mac": "06:00:AC:10:00:02",
      "host_dev_name": "tap0"
    }
  ]
}
EOF

# Start Firecracker
sudo setsid firecracker \
    --config-file /tmp/mvm-snapshot-config.json \
    --api-sock "$SOCKET" \
    --enable-pci \
    </dev/null >/dev/null 2>&1 &
FC_PID=$!

# Wait for socket
for i in $(seq 1 30); do
    sudo test -S "$SOCKET" && break
    sleep 0.1
done

echo "Waiting for template VM to boot and SSH to be ready..."
for i in $(seq 1 120); do
    if sudo ssh -i "$KEY_DIR/mvm.id_ed25519" -o ConnectTimeout=2 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@172.16.0.2 "echo SSH_OK" >/dev/null 2>&1; then
        echo "SSH ready"
        break
    fi
    sleep 0.25
done

# Configure guest networking before snapshot so restored VMs have it ready
echo "Configuring guest networking for snapshot..."
sudo ssh -i "$KEY_DIR/mvm.id_ed25519" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@172.16.0.2 \
    "ip route add default via 172.16.0.1 dev eth0 2>/dev/null; echo 'nameserver 8.8.8.8' > /etc/resolv.conf" 2>/dev/null || true

echo "Creating snapshot..."

# Pause the VM before snapshotting
sudo curl -s --unix-socket "$SOCKET" -X PATCH "http://localhost/vm" \
    -H "Content-Type: application/json" \
    -d '{"state": "Paused"}' >/dev/null

# Create snapshot
sudo curl -s --unix-socket "$SOCKET" -X PUT "http://localhost/snapshot/create" \
    -H "Content-Type: application/json" \
    -d '{
        "snapshot_type": "Full",
        "snapshot_path": "'"$SNAPSHOT_DIR/snapshot_file"'",
        "mem_file_path": "'"$SNAPSHOT_DIR/mem_file"'"
    }' >/dev/null

echo "Snapshot created"

# Kill template VM
sudo kill $FC_PID 2>/dev/null || true
sudo rm -f "$SOCKET" /tmp/mvm-snapshot-config.json
sudo ip link del tap0 2>/dev/null || true

echo "Snapshot files:"
ls -lh "$SNAPSHOT_DIR/"
`, cacheDir, snapshotDir, keyDir,
		cacheDir, snapshotDir)

	out, err := limaClient.ShellScriptWithTimeout(script, lima.LongTimeout*2)
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	fmt.Print(out)
	return nil
}

// HasSnapshot checks if a usable snapshot exists.
func HasSnapshot(limaClient *lima.Client) bool {
	out, _ := limaClient.Shell(fmt.Sprintf("sudo test -f %s/snapshot_file && echo YES || echo NO", SnapshotDir))
	return strings.TrimSpace(out) == "YES"
}

// CopySSHKeyToHost copies the private key from Lima to the host for mvm ssh.
func CopySSHKeyToHost(limaClient *lima.Client, limaKeyDir, hostKeyDir string) error {
	script := fmt.Sprintf("sudo cat %s/mvm.id_ed25519", limaKeyDir)
	keyData, err := limaClient.Shell(script)
	if err != nil {
		return fmt.Errorf("read SSH key: %w", err)
	}

	// Write to host
	if err := writeFileWithPerms(hostKeyDir+"/mvm.id_ed25519", []byte(keyData), 0o600); err != nil {
		return fmt.Errorf("write SSH key to host: %w", err)
	}
	return nil
}
