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

func PoolDir() string { return filepath.Join(DataDir(), "pool") }

func poolSlotDir(i int) string    { return fmt.Sprintf("%s/slot%d", PoolDir(), i) }
func poolPidFile(i int) string    { return poolSlotDir(i) + "/pid" }
func poolReadyFile(i int) string  { return poolSlotDir(i) + "/ready" }
func poolSocketPath(i int) string { return fmt.Sprintf("%s/pool%d.socket", RunDir(), i) }

// Per-slot pristine snapshot files. These are created by the first cold
// boot for that slot and never modified after. Each slot's snapshot has
// its own vsock UDS path embedded (pool{N}.vsock), so snapshots cannot be
// shared across slots — if they were, restoring slotN would try to bind
// slot0's UDS path and fail with "Address in use".
func poolSlotPristineDir(i int) string  { return poolSlotDir(i) + "/pristine" }
func poolSlotPristineSnap(i int) string { return poolSlotPristineDir(i) + "/snapshot_file" }
func poolSlotPristineMem(i int) string  { return poolSlotPristineDir(i) + "/mem_file" }
func poolSlotPristineRoot(i int) string { return poolSlotPristineDir(i) + "/rootfs.ext4" }

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

// WarmPool fills the pool with pre-warmed VMs, sequentially.
// First call: cold boots + creates a golden snapshot with Claude warmed.
// Subsequent calls: restores from snapshot (instant per-slot).
//
// Sequential on purpose: parallel per-slot restore saturates CPU on small
// hosts (4-core n2-standard-4 loses exec latency to pool IO). Callers
// that need speed should claim from the pool (one slot is enough) rather
// than wait for 3/3.
func WarmPool(ex Executor) error {
	warmPoolMu.Lock()
	defer warmPoolMu.Unlock()

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

// isSlotReady returns true only if the POOL_READY marker exists AND the
// slot's FC process is alive. A stale ready file with a dead FC (e.g.,
// after a crash or failed restore) would otherwise cause the pool to
// falsely report the slot as available, and the next mvm start would
// error out with "pool VM not running" or silently hit a zombie UDS.
func isSlotReady(ex Executor, i int) bool {
	out, err := ex.Run(fmt.Sprintf("cat %s 2>/dev/null", poolReadyFile(i)))
	if err != nil || strings.TrimSpace(out) != "POOL_READY" {
		return false
	}
	pidOut, err := ex.Run(fmt.Sprintf("cat %s 2>/dev/null", poolPidFile(i)))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidOut))
	if err != nil || pid <= 0 {
		return false
	}
	return IsRunning(ex, pid)
}

// fillSlot either restores from the slot's own golden snapshot or cold
// boots + creates one. Each slot has its own snapshot because the vsock
// UDS path is embedded in the snapshot — a shared snapshot would make
// restore fail on any slot other than the one that created it.
func fillSlot(ex Executor, i int) error {
	if hasSlotSnapshot(ex, i) {
		return restoreSlotFromSnapshot(ex, i)
	}
	return coldBootAndSnapshot(ex, i)
}

// hasSlotSnapshot returns true only if the slot has a COMPLETE pristine
// snapshot (all three files non-empty). A previous cold-boot that failed
// partway (e.g., snapshot_file + mem_file written but rootfs.ext4 cp
// timed out) would leave an incomplete pristine dir; returning true
// there would cause every future refill to fail with "No such file or
// directory" instead of retrying the cold boot.
func hasSlotSnapshot(ex Executor, i int) bool {
	cmd := fmt.Sprintf(
		"sudo test -s %s && sudo test -s %s && sudo test -s %s && echo YES || echo NO",
		poolSlotPristineSnap(i), poolSlotPristineMem(i), poolSlotPristineRoot(i),
	)
	out, err := ex.Run(cmd)
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

	// Create per-slot pristine snapshot: captures memory (with warm Node.js) +
	// disk state. Pristine files never get mutated after creation — each
	// future refill copies them into the slot's working dir and boots FC
	// against the copy.
	//
	// We split this into discrete steps with per-step error checking
	// because a silent partial failure (e.g., snapshot_file + mem_file
	// created but rootfs copy timed out) would leave the pristine dir
	// incomplete, and the slot would be marked POOL_READY but every
	// future refill would fail with "No such file or directory" on the
	// missing pristine file. A longer timeout (3 min) tolerates IO
	// pressure from other slots warming in parallel.
	fmt.Printf("  Creating pristine snapshot for slot %d...\n", i)
	if _, err := ex.Run(fmt.Sprintf("sudo mkdir -p %s", poolSlotPristineDir(i))); err != nil {
		return fmt.Errorf("slot %d: mkdir pristine: %w", i, err)
	}
	pauseCmd := fmt.Sprintf(`sudo curl -s --unix-socket %s -X PATCH "http://localhost/vm" -H "Content-Type: application/json" -d '{"state": "Paused"}'`, socket)
	if _, err := ex.Run(pauseCmd); err != nil {
		return fmt.Errorf("slot %d: pause: %w", i, err)
	}
	createCmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/create" -H "Content-Type: application/json" -d '{"snapshot_type":"Full","snapshot_path":"%s","mem_file_path":"%s"}'`,
		socket, poolSlotPristineSnap(i), poolSlotPristineMem(i),
	)
	// 5 min timeout: the later slots can take >2 min when earlier pool
	// FCs are active and pageing 2GB each through the same disk. After
	// snapshot creation the FC will be paused and other slots can make
	// progress.
	if _, err := ex.RunWithTimeout(createCmd, 300*time.Second); err != nil {
		return fmt.Errorf("slot %d: snapshot/create: %w", i, err)
	}
	cpRootCmd := fmt.Sprintf("sudo cp --sparse=always %s/rootfs.ext4 %s", poolSlotDir(i), poolSlotPristineRoot(i))
	if _, err := ex.RunWithTimeout(cpRootCmd, 300*time.Second); err != nil {
		return fmt.Errorf("slot %d: cp rootfs: %w", i, err)
	}
	resumeCmd := fmt.Sprintf(`sudo curl -s --unix-socket %s -X PATCH "http://localhost/vm" -H "Content-Type: application/json" -d '{"state": "Resumed"}'`, socket)
	ex.Run(resumeCmd) // best effort — VM is about to be snapshot-restored from anyway
	// Verify all three pristine files exist before marking ready.
	verifyCmd := fmt.Sprintf(`sudo test -s %s && sudo test -s %s && sudo test -s %s && echo PRISTINE_OK`,
		poolSlotPristineSnap(i), poolSlotPristineMem(i), poolSlotPristineRoot(i))
	if out, _ := ex.Run(verifyCmd); !strings.Contains(out, "PRISTINE_OK") {
		return fmt.Errorf("slot %d: pristine files incomplete after cold boot", i)
	}
	fmt.Printf("  Pristine snapshot created for slot %d\n", i)

	// Mark ready — only after ALL pristine files verified.
	ex.Run(fmt.Sprintf(`echo POOL_READY | sudo tee %s >/dev/null`, poolReadyFile(i)))

	return nil
}

// restoreSlotFromSnapshot: fast path — restore from slot's pristine snapshot.
// Each slot has its own pristine snapshot (with its own vsock UDS path
// embedded) so slots refill independently without UDS-bind conflicts.
func restoreSlotFromSnapshot(ex Executor, i int) error {
	fmt.Printf("  Restoring slot %d from pristine snapshot...\n", i)

	alloc := state.AllocateNet(i)

	// Setup TAP + copy rootfs and mem from this slot's pristine dir.
	// FC will write dirty memory pages into mem_file during runtime, so
	// each refill needs a fresh (clean) copy of the pristine mem_file.
	setupCmd := fmt.Sprintf(
		`sudo mkdir -p %s && sudo cp --sparse=always %s %s/rootfs.ext4 && sudo cp --sparse=always %s %s/mem_file && sudo ip link del %s 2>/dev/null; sudo ip tuntap add dev %s mode tap && sudo ip addr add %s/30 dev %s && sudo ip link set dev %s up && sudo touch %s/firecracker.log && sudo chmod 666 %s/firecracker.log && echo SETUP_OK`,
		poolSlotDir(i),
		poolSlotPristineRoot(i), poolSlotDir(i),
		poolSlotPristineMem(i), poolSlotDir(i),
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

	// Restore from pristine snapshot with network_overrides. The snapshot
	// file is immutable (FC only reads it), so we load it directly from
	// the pristine dir. The mem backend points at the copied mem_file
	// which FC will dirty during runtime.
	restoreCmd := fmt.Sprintf(
		`sudo curl -s --unix-socket %s -X PUT "http://localhost/snapshot/load" -H "Content-Type: application/json" -d '{"snapshot_path":"%s","mem_backend":{"backend_path":"%s/mem_file","backend_type":"File"},"enable_diff_snapshots":false,"resume_vm":true,"network_overrides":[{"iface_id":"net1","host_dev_name":"%s"}]}' && echo RESTORE_OK`,
		socket, poolSlotPristineSnap(i), poolSlotDir(i), alloc.TAPDev,
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

	// Move the working files to the VM's dir, but preserve the pristine
	// snapshot so refill can restore from it without re-creating. We
	// explicitly remove only the working/runtime files (ready, pid,
	// mem_file, config.json); the pristine/ subdir stays intact.
	vmDir := VMDir(name)
	moveCmd := fmt.Sprintf(
		`sudo mkdir -p %s && sudo mv %s/rootfs.ext4 %s/rootfs.ext4 && sudo mv %s/firecracker.log %s/firecracker.log && sudo rm -f %s/ready %s/pid %s/mem_file %s/config.json && echo CLAIMED`,
		vmDir, poolSlotDir(slotIdx), vmDir, poolSlotDir(slotIdx), vmDir,
		poolSlotDir(slotIdx), poolSlotDir(slotIdx), poolSlotDir(slotIdx), poolSlotDir(slotIdx),
	)
	out, err = ex.Run(moveCmd)
	if err != nil || !strings.Contains(out, "CLAIMED") {
		return 0, "", fmt.Errorf("failed to claim pool slot")
	}

	// Rename the pool slot's UDS paths to the VM's expected names.
	// rename(2) atomically moves the pathname → inode mapping in the
	// kernel; the FC process keeps its listener socket, but the path
	// that clients connect to is now {name}.vsock.
	//
	// CRITICAL: we MUST rename (not symlink). If we leave pool{N}.vsock
	// in place, ReplenishPool will start a new FC on that path — and
	// `rm -f pool{N}.vsock` in the refill startCmd will unlink our
	// claimed FC's pathname, causing clients to connect to the refilled
	// slot's FC instead (silently running commands on the wrong VM).
	renameCmd := fmt.Sprintf(
		`sudo mv %s/pool%d.vsock %s/%s.vsock && sudo mv %s/pool%d.vsock_5123 %s/%s.vsock_5123 && sudo mv %s/pool%d.socket %s/%s.socket`,
		RunDir(), slotIdx, RunDir(), name,
		RunDir(), slotIdx, RunDir(), name,
		RunDir(), slotIdx, RunDir(), name,
	)
	if _, err := ex.Run(renameCmd); err != nil {
		log.Printf("warning: failed to rename UDS paths for pool slot %d -> %s: %v", slotIdx, name, err)
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

