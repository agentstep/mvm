package firecracker

import (
	"encoding/json"
	"fmt"

	"github.com/agentstep/mvm/internal/state"
)

const (
	CacheDir    = "/opt/mvm/cache"
	VMsDir      = "/opt/mvm/vms"
	KeyDir      = "/opt/mvm/keys"
	RunDir      = "/run/mvm"
	SnapshotDir = "/opt/mvm/snapshot"
)

// SocketPath returns the Firecracker API socket path for a VM.
func SocketPath(name string) string {
	return fmt.Sprintf("%s/%s.socket", RunDir, name)
}

// VsockUDSPath returns the path to the Firecracker vsock-to-Unix-socket
// bridge for a VM. Host→guest connections write "CONNECT <port>\n" to this
// socket and read "OK <hostcid>\n" before the bidirectional stream begins.
//
// This must match the uds_path field set in fcConfig.Vsock (see GenerateConfig).
func VsockUDSPath(name string) string {
	return fmt.Sprintf("%s/%s.vsock", RunDir, name)
}

// VMDir returns the per-VM directory inside Lima.
func VMDir(name string) string {
	return fmt.Sprintf("%s/%s", VMsDir, name)
}

// BootArgs returns optimized kernel boot arguments with network configuration.
// The ip= parameter configures networking at kernel level (before init).
func BootArgs(guestIP, gatewayIP string) string {
	return fmt.Sprintf("keep_bootcon console=ttyS0 reboot=k panic=1 quiet random.trust_cpu=on rootfstype=ext4 ip=%s::%s:255.255.255.252::eth0:off", guestIP, gatewayIP)
}

// fcConfig is the Firecracker JSON configuration file format.
type fcConfig struct {
	BootSource    fcBootSource       `json:"boot-source"`
	Drives        []fcDrive          `json:"drives"`
	NetworkIfaces []fcNetworkIface   `json:"network-interfaces"`
	MachineConfig *fcMachineConfig   `json:"machine-config,omitempty"`
	Vsock         *fcVsock           `json:"vsock,omitempty"`
}

type fcVsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type fcMachineConfig struct {
	VcpuCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type fcNetworkIface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

// Default guest resources. Must not exceed Lima VM's resources.
// 4 vCPU + 2GB is needed for Node.js-based AI agents (Claude Code, Codex)
// which load hundreds of modules at startup.
var (
	GuestVcpuCount  = 4
	GuestMemSizeMiB = 2048
)

// GenerateConfig creates a Firecracker JSON config for a VM.
// cpus=0 and memMB=0 use defaults (GuestVcpuCount, GuestMemSizeMiB).
func GenerateConfig(name string, alloc state.NetAllocation, cpus, memMB int) (string, error) {
	if cpus <= 0 {
		cpus = GuestVcpuCount
	}
	if memMB <= 0 {
		memMB = GuestMemSizeMiB
	}
	cfg := fcConfig{
		BootSource: fcBootSource{
			KernelImagePath: CacheDir + "/vmlinux",
			BootArgs:        BootArgs(alloc.GuestIP, alloc.TAPIP),
		},
		Drives: []fcDrive{
			{
				DriveID:      "rootfs",
				PathOnHost:   VMDir(name) + "/rootfs.ext4",
				IsRootDevice: true,
				IsReadOnly:   false,
			},
		},
		NetworkIfaces: []fcNetworkIface{
			{
				IfaceID:     "net1",
				GuestMAC:    alloc.GuestMAC,
				HostDevName: alloc.TAPDev,
			},
		},
		MachineConfig: &fcMachineConfig{
			VcpuCount:  cpus,
			MemSizeMiB: memMB,
		},
		Vsock: &fcVsock{
			GuestCID: 3, // CID 0-2 reserved by vsock spec
			UDSPath:  fmt.Sprintf("%s/%s.vsock", RunDir, name),
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// StartScript generates a shell script that:
// 1. Sets up OverlayFS rootfs (thin writable layer over shared base)
// 2. Sets up TAP networking
// 3. Writes Firecracker JSON config
// 4. Starts Firecracker with --config-file (no API socket/curl needed)
func StartScript(name string, alloc state.NetAllocation, cpus, memMB int) string {
	socketPath := SocketPath(name)
	vmDir := VMDir(name)

	configJSON, _ := GenerateConfig(name, alloc, cpus, memMB)

	return fmt.Sprintf(`#!/bin/bash
set -e

VM_NAME="%s"
SOCKET_PATH="%s"
VM_DIR="%s"
CACHE_DIR="%s"
RUN_DIR="%s"
TAP_DEV="%s"
TAP_IP="%s"

echo "Preparing microVM '${VM_NAME}'..."

# Create VM directory and copy rootfs (sparse copy — fast, skips zero blocks)
sudo mkdir -p "$VM_DIR"
sudo cp --sparse=always "$CACHE_DIR/base.ext4" "$VM_DIR/rootfs.ext4"
echo "  Rootfs ready"

# Set up TAP device
sudo ip link del "$TAP_DEV" 2>/dev/null || true
sudo ip tuntap add dev "$TAP_DEV" mode tap
sudo ip addr add "${TAP_IP}/30" dev "$TAP_DEV"
sudo ip link set dev "$TAP_DEV" up
echo "  Network: ${TAP_DEV}"

# Create run directory and write config
sudo mkdir -p "$RUN_DIR"
sudo rm -f "$SOCKET_PATH"
sudo rm -f "$RUN_DIR/${VM_NAME}.vsock" "$RUN_DIR/${VM_NAME}.vsock_5123"

cat > "/tmp/mvm-${VM_NAME}.json" <<'FCCONFIG'
%s
FCCONFIG
sudo mv "/tmp/mvm-${VM_NAME}.json" "$VM_DIR/config.json"

# Start Firecracker with config file (no API socket needed for boot)
sudo touch "$VM_DIR/firecracker.log"
sudo chmod 666 "$VM_DIR/firecracker.log"
sudo setsid firecracker \
    --config-file "$VM_DIR/config.json" \
    --api-sock "$SOCKET_PATH" \
    \
    </dev/null >"$VM_DIR/firecracker.log" 2>&1 &
FC_PID=$!

# Brief wait to confirm process started
sleep 0.2
if ! sudo kill -0 $FC_PID 2>/dev/null; then
    echo "ERROR: Firecracker failed to start" >&2
    sudo cat "$VM_DIR/firecracker.log" >&2
    sudo ip link del "$TAP_DEV" 2>/dev/null || true
    exit 1
fi

echo "  VM started (PID: $FC_PID)"
echo "PID:$FC_PID"
`, name, socketPath, vmDir, CacheDir, RunDir,
		alloc.TAPDev, alloc.TAPIP,
		configJSON)
}

// StartScriptWithImage generates a start script that copies from a custom image
// instead of the default base.ext4.
func StartScriptWithImage(name string, alloc state.NetAllocation, cpus, memMB int, imageName string) string {
	socketPath := SocketPath(name)
	vmDir := VMDir(name)

	configJSON, _ := GenerateConfig(name, alloc, cpus, memMB)

	imagePath := CacheDir + "/" + imageName + ".ext4"

	return fmt.Sprintf(`#!/bin/bash
set -e

VM_NAME="%s"
SOCKET_PATH="%s"
VM_DIR="%s"
IMAGE_PATH="%s"
RUN_DIR="%s"
TAP_DEV="%s"
TAP_IP="%s"

echo "Preparing microVM '${VM_NAME}' (image: %s)..."

# Create VM directory and copy custom rootfs (sparse copy — fast, skips zero blocks)
sudo mkdir -p "$VM_DIR"
sudo cp --sparse=always "$IMAGE_PATH" "$VM_DIR/rootfs.ext4"
echo "  Rootfs ready (custom image: %s)"

# Set up TAP device
sudo ip link del "$TAP_DEV" 2>/dev/null || true
sudo ip tuntap add dev "$TAP_DEV" mode tap
sudo ip addr add "${TAP_IP}/30" dev "$TAP_DEV"
sudo ip link set dev "$TAP_DEV" up
echo "  Network: ${TAP_DEV}"

# Create run directory and write config
sudo mkdir -p "$RUN_DIR"
sudo rm -f "$SOCKET_PATH"
sudo rm -f "$RUN_DIR/${VM_NAME}.vsock" "$RUN_DIR/${VM_NAME}.vsock_5123"

cat > "/tmp/mvm-${VM_NAME}.json" <<'FCCONFIG'
%s
FCCONFIG
sudo mv "/tmp/mvm-${VM_NAME}.json" "$VM_DIR/config.json"

# Start Firecracker with config file (no API socket needed for boot)
sudo touch "$VM_DIR/firecracker.log"
sudo chmod 666 "$VM_DIR/firecracker.log"
sudo setsid firecracker \
    --config-file "$VM_DIR/config.json" \
    --api-sock "$SOCKET_PATH" \
    \
    </dev/null >"$VM_DIR/firecracker.log" 2>&1 &
FC_PID=$!

# Brief wait to confirm process started
sleep 0.2
if ! sudo kill -0 $FC_PID 2>/dev/null; then
    echo "ERROR: Firecracker failed to start" >&2
    sudo cat "$VM_DIR/firecracker.log" >&2
    sudo ip link del "$TAP_DEV" 2>/dev/null || true
    exit 1
fi

echo "  VM started (PID: $FC_PID)"
echo "PID:$FC_PID"
`, name, socketPath, vmDir, imagePath, RunDir,
		alloc.TAPDev, alloc.TAPIP,
		imageName, imageName,
		configJSON)
}

// StartExistingScript boots a VM using its existing rootfs (no base.ext4 copy).
// Used after mvm install modifies the rootfs via chroot.
func StartExistingScript(name string, alloc state.NetAllocation, cpus, memMB int) string {
	socketPath := SocketPath(name)
	vmDir := VMDir(name)
	configJSON, _ := GenerateConfig(name, alloc, cpus, memMB)

	return fmt.Sprintf(`#!/bin/bash
set -e

VM_NAME="%s"
SOCKET_PATH="%s"
VM_DIR="%s"
RUN_DIR="%s"
TAP_DEV="%s"
TAP_IP="%s"

echo "Restarting microVM '${VM_NAME}' (existing rootfs)..."

# Verify rootfs exists
if [ ! -f "$VM_DIR/rootfs.ext4" ]; then
    echo "ERROR: rootfs not found at $VM_DIR/rootfs.ext4" >&2
    exit 1
fi

# Set up TAP device
sudo ip link del "$TAP_DEV" 2>/dev/null || true
sudo ip tuntap add dev "$TAP_DEV" mode tap
sudo ip addr add "${TAP_IP}/30" dev "$TAP_DEV"
sudo ip link set dev "$TAP_DEV" up

# Write config
sudo mkdir -p "$RUN_DIR"
sudo rm -f "$SOCKET_PATH"
sudo rm -f "$RUN_DIR/${VM_NAME}.vsock" "$RUN_DIR/${VM_NAME}.vsock_5123"
cat > "/tmp/mvm-${VM_NAME}.json" <<'FCCONFIG'
%s
FCCONFIG
sudo mv "/tmp/mvm-${VM_NAME}.json" "$VM_DIR/config.json"

# Start Firecracker
sudo touch "$VM_DIR/firecracker.log"
sudo chmod 666 "$VM_DIR/firecracker.log"
sudo setsid firecracker \
    --config-file "$VM_DIR/config.json" \
    --api-sock "$SOCKET_PATH" \
    \
    </dev/null >"$VM_DIR/firecracker.log" 2>&1 &
FC_PID=$!

sleep 0.2
if ! sudo kill -0 $FC_PID 2>/dev/null; then
    echo "ERROR: Firecracker failed to start" >&2
    sudo cat "$VM_DIR/firecracker.log" >&2
    sudo ip link del "$TAP_DEV" 2>/dev/null || true
    exit 1
fi

echo "  VM restarted (PID: $FC_PID)"
echo "PID:$FC_PID"
`, name, socketPath, vmDir, RunDir,
		alloc.TAPDev, alloc.TAPIP,
		configJSON)
}

// StartFromSnapshotScript generates a script that restores a VM from a snapshot.
// This skips the entire boot sequence.
func StartFromSnapshotScript(name string, alloc state.NetAllocation) string {
	socketPath := SocketPath(name)
	vmDir := VMDir(name)

	return fmt.Sprintf(`#!/bin/bash
set -e

VM_NAME="%s"
SOCKET_PATH="%s"
VM_DIR="%s"
SNAPSHOT_DIR="%s"
RUN_DIR="%s"
TAP_DEV="%s"
TAP_IP="%s"

echo "Restoring microVM '${VM_NAME}' from snapshot..."

# Create VM directory
sudo mkdir -p "$VM_DIR"

# Copy snapshot memory file (sparse copy)
sudo cp --sparse=always "$SNAPSHOT_DIR/mem_file" "$VM_DIR/mem_file"

# Copy rootfs (sparse)
sudo cp --sparse=always "$SNAPSHOT_DIR/rootfs.ext4" "$VM_DIR/rootfs.ext4"

# Set up TAP device
sudo ip link del "$TAP_DEV" 2>/dev/null || true
sudo ip tuntap add dev "$TAP_DEV" mode tap
sudo ip addr add "${TAP_IP}/30" dev "$TAP_DEV"
sudo ip link set dev "$TAP_DEV" up

# Create run directory
sudo mkdir -p "$RUN_DIR"
sudo rm -f "$SOCKET_PATH"
sudo rm -f "$RUN_DIR/${VM_NAME}.vsock" "$RUN_DIR/${VM_NAME}.vsock_5123"

# Start Firecracker and restore snapshot
sudo touch "$VM_DIR/firecracker.log"
sudo chmod 666 "$VM_DIR/firecracker.log"
sudo setsid firecracker \
    --api-sock "$SOCKET_PATH" \
    \
    </dev/null >"$VM_DIR/firecracker.log" 2>&1 &
FC_PID=$!

# Wait for API socket
for i in $(seq 1 30); do
    sudo test -S "$SOCKET_PATH" && break
    sleep 0.1
done
if ! sudo test -S "$SOCKET_PATH"; then
    echo "ERROR: Firecracker socket not ready" >&2
    sudo kill $FC_PID 2>/dev/null || true
    sudo ip link del "$TAP_DEV" 2>/dev/null || true
    exit 1
fi

# Restore from snapshot with network_overrides to remap TAP device.
# Firecracker v1.13 rejects /network-interfaces after restore, so the
# TAP must be specified inline via network_overrides during load.
sudo curl -s --unix-socket "$SOCKET_PATH" -X PUT "http://localhost/snapshot/load" \
    -H "Content-Type: application/json" \
    -d '{
        "snapshot_path": "'"$SNAPSHOT_DIR/snapshot_file"'",
        "mem_backend": {
            "backend_path": "'"$VM_DIR/mem_file"'",
            "backend_type": "File"
        },
        "enable_diff_snapshots": false,
        "resume_vm": true,
        "network_overrides": [
            {
                "iface_id": "net1",
                "host_dev_name": "'"$TAP_DEV"'"
            }
        ]
    }' || { echo "ERROR: Snapshot restore failed" >&2; sudo kill $FC_PID 2>/dev/null; exit 1; }

echo "  VM restored (PID: $FC_PID)"
echo "PID:$FC_PID"
`, name, socketPath, vmDir, SnapshotDir, RunDir,
		alloc.TAPDev, alloc.TAPIP)
}
