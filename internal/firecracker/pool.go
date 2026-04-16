package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
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

// warmPoolMu serializes WarmPool invocations. Rapid claim bursts (e.g., a
// benchmark doing 3 starts in a row) would otherwise spawn multiple
// ReplenishPool goroutines, each calling WarmPool, each racing to fill
// the same slots. The per-slot isSlotReady check is optimistic — by the
// time fillSlot runs, another invocation may have started filling the
// same slot. Serialize at the top level; parallelism inside WarmPool
// still gives us the full per-slot parallelism benefit.
var warmPoolMu sync.Mutex

// WarmPool fills the pool with pre-warmed VMs.
// First call: cold boots + creates a golden snapshot with Claude warmed.
// Subsequent calls: restores from snapshot (instant).
//
// Empty slots are filled in parallel — each slot takes ~10-20s to restore
// from snapshot, so filling 3 slots sequentially would block the pool
// behind slower refills. Parallel fill uses 3x more CPU briefly but gets
// the pool back to 3/3 in the same time as filling one slot.
//
// The first-ever cold boot + golden snapshot creation must be serial
// (only one VM can write the golden snapshot), so slot 0 runs alone
// when there's no snapshot yet.
func WarmPool(ex Executor) error {
	warmPoolMu.Lock()
	defer warmPoolMu.Unlock()

	// First-ever pool warm: cold boot slot 0 alone (creates golden snapshot),
	// then parallel-fill the rest.
	if !hasGoldenSnapshot(ex) {
		if isSlotReady(ex, 0) {
			// Shouldn't happen, but handle gracefully.
		} else {
			if err := fillSlot(ex, 0); err != nil {
				return fmt.Errorf("cold boot slot 0: %w", err)
			}
		}
	}

	// Parallel fill remaining empty slots.
	var wg sync.WaitGroup
	var mu sync.Mutex
	booted := 0
	var errs []string

	for i := 0; i < PoolSize; i++ {
		if isSlotReady(ex, i) {
			mu.Lock()
			booted++
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			if err := fillSlot(ex, slot); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("slot %d: %v", slot, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			booted++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	for _, e := range errs {
		fmt.Printf("  Warning: pool %s\n", e)
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

	// Wait for the guest agent to be reachable on vsock before marking
	// the slot ready. The snapshot/load call returns as soon as vCPUs
	// resume, but the guest agent needs a moment to re-establish its
	// vsock listener (the UDS path was remapped). Without this wait,
	// the first exec on a pool-claimed VM often fails with "connection
	// refused" for 100-500ms after restore.
	if err := waitForAgent(i, 3*time.Second); err != nil {
		fmt.Printf("  Warning: slot %d agent not responsive after restore: %v\n", i, err)
		// Still mark ready — caller can retry exec if needed. The earlier
		// behavior was "always mark ready", so we're strictly better.
	}

	// Mark ready
	ex.Run(fmt.Sprintf(`echo POOL_READY | sudo tee %s >/dev/null`, poolReadyFile(i)))

	return nil
}

// waitForAgent polls the in-guest mvm-agent via vsock until it responds
// or the deadline passes. Used after snapshot restore to ensure pool
// slots don't report "ready" while their agent is still reconnecting.
func waitForAgent(slotIdx int, timeout time.Duration) error {
	udsPath := fmt.Sprintf("%s/pool%d.vsock", RunDir(), slotIdx)
	client := agentclient.New(&agentclient.FirecrackerVsockDialer{UDSPath: udsPath})

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := client.Ping(ctx)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("agent not responsive within %v", timeout)
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

	// Symlink the VM's expected UDS paths to the pool slot's actual UDS
	// paths. Firecracker was started with pool{N}.socket / pool{N}.vsock
	// and we can't change those without restarting — but exec expects
	// {name}.vsock. Symlinks bridge the gap transparently.
	linkCmd := fmt.Sprintf(
		`sudo ln -sf pool%d.vsock %s/%s.vsock && sudo ln -sf pool%d.vsock_5123 %s/%s.vsock_5123 && sudo ln -sf pool%d.socket %s/%s.socket`,
		slotIdx, RunDir(), name,
		slotIdx, RunDir(), name,
		slotIdx, RunDir(), name,
	)
	if _, err := ex.Run(linkCmd); err != nil {
		log.Printf("warning: failed to symlink UDS paths for pool slot %d -> %s: %v", slotIdx, name, err)
	}

	// The slot was marked ready during fillSlot which already waited for
	// the agent. But changing the TAP device triggers a new network config
	// via agent.Exec in the post-boot goroutine, which briefly interrupts
	// the agent. Wait one more ping to make sure the caller can exec
	// immediately after start returns.
	if err := waitForAgent(slotIdx, 2*time.Second); err != nil {
		log.Printf("warning: agent not responsive after claim: %v", err)
		// Don't fail the claim — the VM is usable, exec retries are cheap.
	}

	return pid, SocketPath(name), nil
}

// ReplenishPool boots new warm VMs in the background to fill the pool.
func ReplenishPool(ex Executor) {
	go func() {
		_ = WarmPool(ex)
	}()
}

