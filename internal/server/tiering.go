package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/agentstep/mvm/internal/egressdns"
	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
)

// Idle tiering: pause the recently-idle, archive the long-idle.
//
// WHY THIS IS IN THE DAEMON. The existing auto-idle is a launchd agent installed by `mvm idle
// enable` — a macOS developer convenience that also guards on Lima being up, so it never runs on a
// Linux server. A host packing sandboxes needs the policy where the VMs are.
//
// WHY TWO TIERS. Pause freezes vCPUs and leaves RAM untouched, so it bounds CPU but not the
// resource that actually limits density. It is the right trade over a short horizon — an agent
// doing exec, think, exec seconds apart resumes instantly and holding its memory for a minute costs
// little. Over a long horizon it is the wrong one: a sandbox idle for an hour has no business
// occupying memory. Archive checkpoints it to disk and gives the RAM back, at the cost of a slower
// restore. Neither is better; they suit different horizons, which is why both exist and the
// thresholds are per-VM.
const (
	// DefaultTieringInterval is how often the sweep runs. Coarse on purpose: the thresholds it
	// enforces are minutes to hours, and every pass walks all VMs.
	DefaultTieringInterval = 30 * time.Second
)

// runTieringLoop sweeps until the context is cancelled. Errors are logged and the loop continues —
// one VM that cannot be archived must not stop the others from being.
func (s *Server) runTieringLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultTieringInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tierOnce(time.Now())
		}
	}
}

// tierOnce applies both thresholds to every VM. Separated from the loop, and taking `now`, so a
// test can drive it directly without waiting on a ticker.
func (s *Server) tierOnce(now time.Time) {
	st, err := s.store.Load()
	if err != nil {
		log.Printf("tiering: load state: %v", err)
		return
	}

	for _, vm := range st.VMs {
		// Firecracker only, for now. Both tiers call firecracker.* directly — Pause here and
		// SnapshotVM inside archiveVM — and every handler in routes.go that touches a VM branches
		// on vm.Backend == "applevz" first. Without this guard the sweep calls the wrong backend's
		// code on an applevz VM, and it does so UNATTENDED, which is worse than a handler doing it
		// in response to an explicit request.
		//
		// applevz can pause (internal/vzhelper/client.go's Pause is a memory-resident freeze), so
		// this is a wiring gap rather than a capability one: tiering it needs the vzhelper path
		// here and an applevz equivalent of SnapshotVM for the archive tier.
		if vm.Backend != "" && vm.Backend != "firecracker" {
			continue
		}

		// A command is running inside it. LastActivity is stamped when an exec starts, and exec
		// allows five minutes, so an idle_timeout below that would otherwise freeze a build or an
		// install while it is still working — indistinguishable, from inside the guest, from the
		// host hanging. beginExec/busy is what tells the sweep the difference between a VM nobody
		// is using and one in the middle of a long command.
		if s.ops.busy(vm.Name) {
			continue
		}

		// A VM that has never reported activity has no idle age to measure. Falling back to
		// CreatedAt would archive a freshly created VM that simply has not been used yet.
		if vm.LastActivity == nil {
			continue
		}
		idle := now.Sub(*vm.LastActivity)

		switch vm.Status {
		case "running":
			d, ok := parseThreshold(vm.IdleTimeout)
			if ok && idle >= d {
				// Under the VM's transition lock, and re-checking busy inside it: an exec that
				// started between the check above and here holds the lock through its own restore
				// step, so this blocks rather than pausing a VM a request just woke.
				err := s.ops.with(vm.Name, func() error {
					if s.ops.busy(vm.Name) {
						return errVMBusy
					}
					if err := firecracker.Pause(s.executor, vm); err != nil {
						return err
					}
					s.store.UpdateVM(vm.Name, func(v *state.VM) { v.Status = "paused" })
					return nil
				})
				if err != nil {
					if err != errVMBusy {
						log.Printf("tiering: pause %s: %v", vm.Name, err)
					}
					continue
				}
				log.Printf("tiering: paused %s after %s idle (vCPUs frozen, %dMB still held)", vm.Name, idle.Round(time.Second), vm.MemoryMB)
			}

		case "paused":
			d, ok := parseThreshold(vm.ArchiveAfter)
			if ok && idle >= d {
				// Archive is the destructive tier — it snapshots and then kills — so it must never
				// interleave with a restore or an exec on the same VM.
				err := s.ops.with(vm.Name, func() error {
					if s.ops.busy(vm.Name) {
						return errVMBusy
					}
					_, aerr := s.archiveVM(vm)
					return aerr
				})
				if err != nil {
					if err != errVMBusy {
						log.Printf("tiering: archive %s: %v", vm.Name, err)
					}
					continue
				}
				log.Printf("tiering: archived %s after %s idle (%dMB released)", vm.Name, idle.Round(time.Second), vm.MemoryMB)
			}
		}
	}
}

// parseThreshold reads a duration like "5m". An empty or unparseable value disables the tier
// rather than falling back to a default: silently applying a threshold nobody asked for is how a
// running sandbox disappears.
// errVMBusy means a command started on the VM between the sweep's check and its transition. Not a
// failure — the VM is in use, which is the whole reason not to tier it — so it is not logged.
var errVMBusy = errors.New("vm busy")

func parseThreshold(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// abandonRestoredVM stops a VM that came back but could not be constrained, and returns its record
// to "archived" so a later attempt can retry from the same checkpoint.
//
// Mirrors archiveVM's teardown: kill the process, drop the socket and TAP, reap the UFFD handler.
// Best-effort throughout — every step is already "|| true" shaped — because the alternative to a
// partial cleanup is a fully live VM, which is strictly worse.
func (s *Server) abandonRestoredVM(name string, pid, uffdPid int, alloc state.NetAllocation, preserved state.VM) {
	firecracker.RemoveEgressPolicy(s.executor, &state.VM{Name: name, NetIndex: alloc.Index, TAPDevice: alloc.TAPDev, GuestIP: alloc.GuestIP})
	s.dns.Stop(alloc.Index)
	s.executor.Run(fmt.Sprintf("sudo kill -9 %d 2>/dev/null || true", pid))
	s.executor.Run(fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true",
		firecracker.SocketPath(name), alloc.TAPDev))
	if uffdPid != 0 {
		firecracker.KillUFFDHandler(uffdPid)
	}

	// Back to archived, with the snapshot still named: the checkpoint on disk is untouched, so the
	// sandbox is recoverable rather than lost.
	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "archived"
		v.ArchivedSnapshot = preserved.ArchivedSnapshot
		v.PID = 0
		v.UFFDPid = 0
	})
	log.Printf("tiering: VM %s restored but could not be constrained; stopped and returned to archived (checkpoint %s kept)", name, preserved.ArchivedSnapshot)
}

// policyForRestore decides what egress policy a returning VM gets.
//
// Its whole job is the failure case. The stored NetPolicy string is the only surviving record of
// what the VM was created with, and if it no longer parses there are two options: hand back a VM
// with no filter, or hand back a closed one. Open is unrecoverable — untrusted guest code is
// already running by the time anyone notices — so this fails closed, matching handleStartVM.
func policyForRestore(name, stored string) state.ParsedNetPolicy {
	policy, err := state.ParseNetPolicy(stored)
	if err != nil {
		log.Printf("tiering: VM %s has unparseable stored policy %q, failing closed to deny: %v", name, stored, err)
		return state.ParsedNetPolicy{Mode: state.NetPolicyDeny}
	}
	return policy
}

// restoreArchivedVM brings an archived VM back from its checkpoint, in place.
//
// This is what makes archiving invisible to callers. Without it, tiering is only safe if every
// caller knows to restore first — which turns a memory optimisation into an API contract change.
//
// PRESERVING CONFIG IS THE WHOLE DIFFICULTY. RestoreVMSnapshot works by removing the VM entry and
// reserving a fresh one with a new network allocation, so everything the record carried is dropped:
// the two idle thresholds, the declarative spec, attached secrets, port maps, the egress policy,
// its CPU and memory shape. Restoring without putting them back would silently return a VM that
// never tiers again, has no secrets, and forwards no ports — the same VM by name, quietly not the
// same VM. So the config is captured before the restore and written back after it.
func (s *Server) restoreArchivedVM(vm *state.VM) error {
	if vm.ArchivedSnapshot == "" {
		return fmt.Errorf("VM %q is archived but records no snapshot — its memory is gone and nothing says where the state went", vm.Name)
	}

	snapDir := filepath.Join(snapshotsBaseDir(), vm.ArchivedSnapshot)
	if _, err := os.Stat(filepath.Join(snapDir, "meta.json")); err != nil {
		return fmt.Errorf("archive %q for VM %q not found: %w", vm.ArchivedSnapshot, vm.Name, err)
	}

	// Captured BEFORE the restore, because reserving a fresh entry discards all of it.
	preserved := *vm
	name := vm.Name

	// ArchivedSnapshot goes on the reserved record immediately, not after the restore succeeds.
	// Between RemoveVM and the final update the daemon can die, and a record left in "restoring"
	// with an empty ArchivedSnapshot fails this function's own opening guard forever — the
	// checkpoint is still on disk, but nothing records where. Writing it up front means any crash
	// leaves enough to recover from.
	s.store.RemoveVM(name)
	netIndex, err := s.store.ReserveVM(&state.VM{
		Name:             name,
		Status:           "restoring",
		CreatedAt:        preserved.CreatedAt,
		ArchivedSnapshot: preserved.ArchivedSnapshot,
	})
	if err != nil {
		return fmt.Errorf("reserve %q for restore: %w", name, err)
	}
	alloc := state.AllocateNet(netIndex)

	pid, socketPath, uffdPid, err := firecracker.RestoreVMSnapshot(s.executor, name, snapDir, alloc)
	if err != nil {
		// Leave the record behind rather than removing it: the checkpoint on disk is still valid,
		// so a later attempt can succeed. Removing the entry would strand a recoverable sandbox.
		s.store.UpdateVM(name, func(v *state.VM) {
			v.Status = "archived"
			v.ArchivedSnapshot = preserved.ArchivedSnapshot
		})
		return fmt.Errorf("restore %q from %q: %w", name, preserved.ArchivedSnapshot, err)
	}

	// REINSTALL ENFORCEMENT, not just the metadata.
	//
	// archiveVM tore down the host-side egress rules and the DNS allowlist resolver on the way out
	// (RemoveEgressPolicy + dns.Stop). Copying the NetPolicy STRING back into the record below
	// restores what `inspect` reports and none of what actually constrains the guest — so a sandbox
	// created with `deny` or `allow:github.com` would come back with unrestricted egress while
	// still claiming to be restricted. On a host running untrusted code that is a containment
	// breach, and a silent one.
	//
	// Fails closed exactly as handleStartVM does: a stored policy string that no longer parses
	// becomes deny, never open.
	// From here the microVM is RUNNING. Any failure below must tear it down before returning:
	// abandoning it leaves a live Firecracker process — with a guest that is executing, on a
	// network allocation the record no longer claims — that nothing will ever reap. Refusing to
	// hand back an unconstrained VM is only safe if refusing also stops it.
	policy := policyForRestore(name, preserved.NetPolicy)
	if err := firecracker.InstallEgressPolicy(s.executor, alloc, policy); err != nil {
		s.abandonRestoredVM(name, pid, uffdPid, alloc, preserved)
		return fmt.Errorf("restore %q: egress policy not reinstalled, VM stopped rather than left unconstrained: %w", name, err)
	}
	if policy.Mode == state.NetPolicyAllow {
		sets := egressdns.SetPair{
			V4: firecracker.EgressIPSetName(alloc.Index),
			V6: firecracker.EgressIPSetName6(alloc.Index),
		}
		if err := s.dns.Start(alloc, sets, policy.Domains); err != nil {
			s.abandonRestoredVM(name, pid, uffdPid, alloc, preserved)
			return fmt.Errorf("restore %q: egress DNS not restarted, VM stopped rather than left unconstrained: %w", name, err)
		}
	}

	now := time.Now()
	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = alloc.TAPDev
		v.GuestMAC = alloc.GuestMAC
		v.SocketPath = socketPath
		v.PID = pid
		v.UFFDPid = uffdPid
		v.RootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"

		// Put back everything the fresh reservation dropped.
		v.IdleTimeout = preserved.IdleTimeout
		v.ArchiveAfter = preserved.ArchiveAfter
		v.Spec = preserved.Spec
		v.Secrets = preserved.Secrets
		v.Ports = preserved.Ports
		v.NetPolicy = preserved.NetPolicy
		v.Backend = preserved.Backend
		v.Cpus = preserved.Cpus
		v.MemoryMB = preserved.MemoryMB

		// It is running again, so it is no longer archived, and this access is activity — without
		// resetting the clock the next sweep would archive it straight back.
		v.ArchivedSnapshot = ""
		v.StoppedAt = nil
		v.LastActivity = &now
	})

	// Published-port DNAT was removed by archiveVM too, and the rules reference the TAP/IP of the
	// OLD allocation, so they have to be reinstalled against the new one. Non-fatal: the VM is
	// running and constrained, and a missing port forward is a connectivity problem, not a
	// containment one.
	if len(preserved.Ports) > 0 {
		restored, err := s.store.GetVM(name)
		if err == nil {
			if perr := firecracker.SetupPortForwarding(s.executor, restored); perr != nil {
				log.Printf("tiering: VM %s restored but port forwarding failed: %v", name, perr)
			}
		}
	}
	return nil
}
