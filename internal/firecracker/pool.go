package firecracker

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

const (
	PoolSize = 3 // pre-boot up to 3 VMs for concurrent sessions
)

func PoolDir() string         { return filepath.Join(DataDir(), "pool") }
func poolSnapshotDir() string { return filepath.Join(PoolDir(), "snapshot") }

func poolSlotDir(i int) string    { return fmt.Sprintf("%s/slot%d", PoolDir(), i) }
func poolPidFile(i int) string    { return poolSlotDir(i) + "/pid" }
func poolReadyFile(i int) string  { return poolSlotDir(i) + "/ready" }
func poolSocketPath(i int) string { return fmt.Sprintf("%s/pool%d.socket", RunDir(), i) }

func PoolSocketPathForSlot(i int) string { return poolSocketPath(i) }
func PoolSocketPath() string             { return poolSocketPath(0) }

// WarmPool fills the pool with pre-warmed VMs.
// First call: cold boots + creates a golden snapshot with Claude warmed.
// Subsequent calls: restores from snapshot (instant).
func WarmPool(ex Executor) error {
	booted := 0
	for i := 0; i < PoolSize; i++ {
		if isSlotReady(ex, i) {
			booted++
			continue
		}
		if err := fillSlot(ex, i); err != nil {
			fmt.Printf("  Warning: pool slot %d failed: %v\n", i, err)
			continue
		}
		booted++
	}
	if booted == 0 {
		return fmt.Errorf("no pool VMs booted")
	}
	return nil
}

func isSlotReady(ex Executor, i int) bool {
	out, err := ex.Run(fmt.Sprintf("cat %s 2>/dev/null", poolReadyFile(i)))
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "POOL_READY"
}

// fillSlot either restores from golden snapshot or cold boots + creates one.
func fillSlot(ex Executor, i int) error {
	if hasGoldenSnapshot(ex) {
		return restoreSlotFromSnapshot(ex, i)
	}
	return coldBootAndSnapshot(ex, i)
}

func hasGoldenSnapshot(ex Executor) bool {
	out, err := ex.Run(fmt.Sprintf("sudo test -f %s/snapshot_file && echo YES || echo NO", poolSnapshotDir()))
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "YES"
}

// coldBootAndSnapshot: first-time pool warm.
// Cold boots a VM, waits for SSH, pre-warms Claude, then snapshots.
func coldBootAndSnapshot(ex Executor, i int) error {
	fmt.Println("  First pool warm: cold booting + creating golden snapshot...")

	alloc := state.AllocateNet(i)

	// Build and write config
	cfg := fcConfig{
		BootSource: fcBootSource{
			KernelImagePath: CacheDir() + "/vmlinux",
			BootArgs:        BootArgs(alloc.GuestIP, alloc.TAPIP),
		},
		Drives: []fcDrive{{
			DriveID:      "rootfs",
			PathOnHost:   poolSlotDir(i) + "/rootfs.ext4",
			IsRootDevice: true,
			IsReadOnly:   false,
		}},
		NetworkIfaces: []fcNetworkIface{{
			IfaceID:     "net1",
			GuestMAC:    alloc.GuestMAC,
			HostDevName: alloc.TAPDev,
		}},
		MachineConfig: &fcMachineConfig{
			VcpuCount:  GuestVcpuCount,
			MemSizeMiB: GuestMemSizeMiB,
		},
		Vsock: &fcVsock{
			GuestCID: 3,
			UDSPath:  fmt.Sprintf("%s/pool%d.vsock", RunDir(), i),
		},
	}
	cfgJSON, _ := json.Marshal(cfg)
	cfgEscaped := strings.ReplaceAll(string(cfgJSON), `"`, `\"`)

	writeConfig := fmt.Sprintf(
		`sudo mkdir -p %s && echo "%s" | sudo tee %s/config.json >/dev/null && echo CFG_OK`,
		poolSlotDir(i), cfgEscaped, poolSlotDir(i),
	)
	out, err := ex.Run(writeConfig)
	if err != nil || !strings.Contains(out, "CFG_OK") {
		return fmt.Errorf("config write failed: %w", err)
	}

	// Setup rootfs and TAP
	setup := fmt.Sprintf(
		`sudo cp --sparse=always %s/base.ext4 %s/rootfs.ext4 && sudo ip link del %s 2>/dev/null; sudo ip tuntap add dev %s mode tap && sudo ip addr add %s/30 dev %s && sudo ip link set dev %s up && sudo touch %s/firecracker.log && sudo chmod 666 %s/firecracker.log && echo SETUP_OK`,
		CacheDir(), poolSlotDir(i),
		alloc.TAPDev, alloc.TAPDev, alloc.TAPIP, alloc.TAPDev, alloc.TAPDev,
		poolSlotDir(i), poolSlotDir(i),
	)
	out, err = ex.RunWithTimeout(setup, LongTimeout)
	if err != nil || !strings.Contains(out, "SETUP_OK") {
		return fmt.Errorf("setup failed: %w", err)
	}

	// Start Firecracker
	socket := poolSocketPath(i)
	startCmd := fmt.Sprintf(
		`sudo rm -f %s %s/pool%d.vsock %s/pool%d.vsock_5123; sudo setsid firecracker --config-file %s/config.json --api-sock %s</dev/null >%s/firecracker.log 2>&1 & echo $! | sudo tee %s/pid`,
		socket, RunDir(), i, RunDir(), i, poolSlotDir(i), socket, poolSlotDir(i), poolSlotDir(i),
	)
	out, err = ex.Run(startCmd)
	if err != nil {
		return fmt.Errorf("FC start failed: %w", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(out))
	fmt.Printf("  Pool slot %d starting (PID: %d)...\n", i, pid)

	// Wait for agent readiness
	if !WaitForGuest(ex, alloc.GuestIP, 90*time.Second) {
		return fmt.Errorf("agent not ready")
	}
	SetupGuestNetworkViaAgent(ex, alloc.GuestIP, alloc.TAPIP)

	// Pre-warm Claude CLI via agent — loads Node.js + all modules into VM memory
	fmt.Println("  Pre-warming Claude CLI (loading Node.js modules into memory)...")
	// Pre-warm Claude CLI via agent (direct TCP exec)
	agentExec(ex, alloc.GuestIP, "command -v claude >/dev/null 2>&1 && NODE_COMPILE_CACHE=/tmp/v8-cache claude --version >/dev/null 2>&1 || true")

	// Create golden snapshot: captures memory (with warm Node.js) + disk state
	fmt.Println("  Creating golden snapshot...")
	snapCmd := fmt.Sprintf(
		`sudo mkdir -p %s && sudo curl -s --unix-socket %s -X PATCH "http://localhost/vm" -H "Content-Type: application/json" -d '{"state": "Paused"}' && sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/create" -H "Content-Type: application/json" -d '{"snapshot_type":"Full","snapshot_path":"%s/snapshot_file","mem_file_path":"%s/mem_file"}' && sudo cp --sparse=always %s/rootfs.ext4 %s/rootfs.ext4 && sudo curl -s --unix-socket %s -X PATCH "http://localhost/vm" -H "Content-Type: application/json" -d '{"state": "Resumed"}' && echo SNAP_OK`,
		poolSnapshotDir(), socket, socket,
		poolSnapshotDir(), poolSnapshotDir(),
		poolSlotDir(i), poolSnapshotDir(),
		socket,
	)
	out, err = ex.RunWithTimeout(snapCmd, 60*time.Second)
	if err != nil || !strings.Contains(out, "SNAP_OK") {
		fmt.Printf("  Warning: snapshot creation failed (pool will cold-boot next time): %v\n", err)
	} else {
		fmt.Println("  Golden snapshot created")
	}

	// Mark ready
	ex.Run(fmt.Sprintf(`echo POOL_READY | sudo tee %s >/dev/null`, poolReadyFile(i)))

	return nil
}

// restoreSlotFromSnapshot: fast path — restore from golden snapshot.
// This gives a VM with Claude already warmed in memory.
func restoreSlotFromSnapshot(ex Executor, i int) error {
	fmt.Println("  Restoring from golden snapshot (instant Claude-ready VM)...")

	alloc := state.AllocateNet(i)

	// Setup TAP + copy rootfs and mem from snapshot
	setupCmd := fmt.Sprintf(
		`sudo mkdir -p %s && sudo cp --sparse=always %s/rootfs.ext4 %s/rootfs.ext4 && sudo cp --sparse=always %s/mem_file %s/mem_file && sudo ip link del %s 2>/dev/null; sudo ip tuntap add dev %s mode tap && sudo ip addr add %s/30 dev %s && sudo ip link set dev %s up && sudo touch %s/firecracker.log && sudo chmod 666 %s/firecracker.log && echo SETUP_OK`,
		poolSlotDir(i),
		poolSnapshotDir(), poolSlotDir(i),
		poolSnapshotDir(), poolSlotDir(i),
		alloc.TAPDev, alloc.TAPDev, alloc.TAPIP, alloc.TAPDev, alloc.TAPDev,
		poolSlotDir(i), poolSlotDir(i),
	)
	out, err := ex.RunWithTimeout(setupCmd, LongTimeout)
	if err != nil || !strings.Contains(out, "SETUP_OK") {
		return fmt.Errorf("snapshot restore setup failed: %w", err)
	}

	// Start Firecracker (API-only mode for snapshot restore)
	socket := poolSocketPath(i)
	startCmd := fmt.Sprintf(
		`sudo rm -f %s %s/pool%d.vsock %s/pool%d.vsock_5123; sudo mkdir -p %s; sudo setsid firecracker --api-sock %s</dev/null >%s/firecracker.log 2>&1 & echo $! | sudo tee %s/pid`,
		socket, RunDir(), i, RunDir(), i, RunDir(), socket, poolSlotDir(i), poolSlotDir(i),
	)
	out, err = ex.Run(startCmd)
	if err != nil {
		return fmt.Errorf("FC start failed: %w", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(out))

	// Wait for API socket
	waitSocket := fmt.Sprintf(`for j in $(seq 1 30); do sudo test -S %s && break; sleep 0.1; done; sudo test -S %s && echo SOCK_OK`, socket, socket)
	out, err = ex.Run(waitSocket)
	if err != nil || !strings.Contains(out, "SOCK_OK") {
		return fmt.Errorf("API socket not ready")
	}

	// Restore from snapshot with network_overrides
	restoreCmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/load" -H "Content-Type: application/json" -d '{"snapshot_path":"%s/snapshot_file","mem_backend":{"backend_path":"%s/mem_file","backend_type":"File"},"enable_diff_snapshots":false,"resume_vm":true,"network_overrides":[{"iface_id":"net1","host_dev_name":"%s"}]}' && echo RESTORE_OK`,
		socket, poolSnapshotDir(), poolSlotDir(i), alloc.TAPDev,
	)
	out, err = ex.RunWithTimeout(restoreCmd, 30*time.Second)
	if err != nil || !strings.Contains(out, "RESTORE_OK") {
		return fmt.Errorf("snapshot restore failed: %v (output: %s)", err, out)
	}

	fmt.Printf("  Pool slot %d restored (PID: %d)\n", i, pid)

	// Mark ready — no need to wait for SSH, the snapshot was taken post-SSH
	ex.Run(fmt.Sprintf(`echo POOL_READY | sudo tee %s >/dev/null`, poolReadyFile(i)))

	return nil
}

// IsPoolReady checks if any warm VM is available.
func IsPoolReady(ex Executor) bool {
	for i := 0; i < PoolSize; i++ {
		if isSlotReady(ex, i) {
			return true
		}
	}
	return false
}

// PoolStatus returns (ready, total) counts.
func PoolStatus(ex Executor) (int, int) {
	ready := 0
	for i := 0; i < PoolSize; i++ {
		if isSlotReady(ex, i) {
			ready++
		}
	}
	return ready, PoolSize
}

// ClaimPoolSlot takes a ready pool VM and reconfigures its network for the
// requested allocation. Returns (pid, socketPath, error).
func ClaimPoolSlot(ex Executor, name string, alloc state.NetAllocation) (int, string, error) {
	slotIdx := -1
	for i := 0; i < PoolSize; i++ {
		if isSlotReady(ex, i) {
			slotIdx = i
			break
		}
	}
	if slotIdx == -1 {
		return 0, "", fmt.Errorf("no warm VM available")
	}

	out, err := ex.Run(fmt.Sprintf("cat %s", poolPidFile(slotIdx)))
	if err != nil {
		return 0, "", err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || !IsRunning(ex, pid) {
		return 0, "", fmt.Errorf("pool VM not running")
	}

	// Reconfigure network if needed (pool VMs boot with slot's own TAP)
	slotAlloc := state.AllocateNet(slotIdx)
	if alloc.Index != slotIdx {
		// Set up the requested TAP device and reconfigure guest networking
		tapSetup := fmt.Sprintf(
			`sudo ip link del %s 2>/dev/null; sudo ip tuntap add dev %s mode tap && sudo ip addr add %s/30 dev %s && sudo ip link set dev %s up`,
			alloc.TAPDev, alloc.TAPDev, alloc.TAPIP, alloc.TAPDev, alloc.TAPDev,
		)
		ex.Run(tapSetup)
		// Clean up old TAP
		if slotAlloc.TAPDev != alloc.TAPDev {
			ex.Run(fmt.Sprintf("sudo ip link del %s 2>/dev/null || true", slotAlloc.TAPDev))
		}
	}

	vmDir := VMDir(name)
	moveCmd := fmt.Sprintf(
		`sudo mkdir -p %s && sudo mv %s/rootfs.ext4 %s/rootfs.ext4 && sudo mv %s/firecracker.log %s/firecracker.log && sudo rm -rf %s && echo CLAIMED`,
		vmDir, poolSlotDir(slotIdx), vmDir, poolSlotDir(slotIdx), vmDir, poolSlotDir(slotIdx),
	)
	out, err = ex.Run(moveCmd)
	if err != nil || !strings.Contains(out, "CLAIMED") {
		return 0, "", fmt.Errorf("failed to claim pool slot")
	}

	return pid, poolSocketPath(slotIdx), nil
}

// ReplenishPool boots new warm VMs in the background to fill the pool.
func ReplenishPool(ex Executor) {
	go func() {
		_ = WarmPool(ex)
	}()
}

