package handler

import "sync"

// maxPendingStatuses bounds the cache of statuses reaped before anyone claimed
// them. Orphans — every detached `mvm exec -d` job — land here and are never
// claimed, so this must be capped or it grows for the life of the VM. Real
// sessions claim their status within microseconds, so a small cache is ample.
const maxPendingStatuses = 256

type pendingStatus struct {
	status int
	seq    uint64
}

// statusRegistry routes child exit statuses from a single wait4(-1) reaper to
// whichever session is waiting on that pid.
//
// It exists because reaping and os/exec cannot naively coexist. mvm-agent runs
// as PID 1, so orphaned processes reparent to it and must be reaped or they
// become permanent zombies — every `mvm exec -d` job leaks one today. But a
// bare wait4(-1) loop running alongside cmd.Wait() will sometimes collect a
// session's child first; cmd.Wait() then fails with ECHILD and the exit code is
// lost. Combined with the old handler code, which reported 0 for any
// non-ExitError, that surfaced as a failed command reporting SUCCESS —
// intermittently, under load, which is the worst way to find out.
//
// The fix is to have exactly one owner of wait4 results per namespace, and have
// sessions collect their status from here rather than racing for it.
//
// Rejected alternative: pausing the reaper while any session runs. A single
// long-lived `mvm exec -it bash` would then let zombies accumulate without
// bound for as long as the shell is open.
type statusRegistry struct {
	mu      sync.Mutex
	waiters map[int]chan int
	// pending holds statuses reaped before register() ran. Without it there is
	// an unavoidable race: a child can exit between Start() returning and its
	// pid being registered, in which case deliver() would find no waiter,
	// discard the status, and the session would then block forever on a
	// channel nothing will ever send to.
	pending map[int]pendingStatus
	seq     uint64
}

func newStatusRegistry() *statusRegistry {
	return &statusRegistry{
		waiters: make(map[int]chan int),
		pending: make(map[int]pendingStatus),
	}
}

// register claims a pid and returns the channel its status will arrive on.
//
// If the pid was already reaped (the race described on pending), the returned
// channel is pre-filled, so the caller sees the status immediately rather than
// hanging.
func (r *statusRegistry) register(pid int) chan int {
	ch := make(chan int, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if ps, ok := r.pending[pid]; ok {
		delete(r.pending, pid)
		ch <- ps.status
		return ch
	}
	r.waiters[pid] = ch
	return ch
}

// unregister drops a pid's waiter. Safe to call more than once.
func (r *statusRegistry) unregister(pid int) {
	r.mu.Lock()
	delete(r.waiters, pid)
	r.mu.Unlock()
}

// deliver hands a reaped pid's status to its waiter, caching it if the waiter
// has not registered yet.
//
// Never blocks: blocking here would stall reaping for the entire namespace on
// one abandoned session.
func (r *statusRegistry) deliver(pid, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ch, ok := r.waiters[pid]; ok {
		select {
		case ch <- status:
		default: // already satisfied
		}
		return
	}

	r.seq++
	r.pending[pid] = pendingStatus{status: status, seq: r.seq}
	r.evictOldestLocked()
}

// evictOldestLocked drops the least-recently-added pending status once the
// cache is over cap. Callers hold r.mu.
func (r *statusRegistry) evictOldestLocked() {
	if len(r.pending) <= maxPendingStatuses {
		return
	}
	oldestPid, oldestSeq := 0, ^uint64(0)
	for pid, ps := range r.pending {
		if ps.seq < oldestSeq {
			oldestPid, oldestSeq = pid, ps.seq
		}
	}
	delete(r.pending, oldestPid)
}

// len reports how many pids are currently claimed. Test helper for leak checks.
func (r *statusRegistry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waiters)
}

// pendingLen reports the size of the unclaimed-status cache. Test helper.
func (r *statusRegistry) pendingLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
