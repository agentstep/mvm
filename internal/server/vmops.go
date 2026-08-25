package server

import (
	"sync"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// vmOps serialises the state transitions that can race for a single VM, and tracks which VMs have
// a command running inside them.
//
// Two distinct hazards, both from the tiering sweep running unattended alongside request handlers:
//
//  1. Overlapping transitions. The sweep archives (snapshot, then kill) while an exec restores the
//     same VM; each reads a status, decides, and writes, with no ordering between them. The
//     outcomes range from a wasted snapshot to a VM killed moments after being restored — and
//     because archive removes the state entry and restore reserves a fresh allocation, an
//     interleaving can leave a running Firecracker process with no record pointing at it.
//
//  2. A command paused mid-run. LastActivity is stamped when an exec STARTS. Exec allows five
//     minutes, so with idle_timeout below that, a build or an install gets its vCPUs frozen while
//     it is still working. The sweep sees an idle VM because nothing has told it otherwise.
//
// The lock is held only across transitions, never across an exec — a five-minute command must not
// block the sweep from touching other VMs, and per-VM locks mean it does not.
type vmOps struct {
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	inFlight map[string]int
}

func newVMOps() *vmOps {
	return &vmOps{locks: map[string]*sync.Mutex{}, inFlight: map[string]int{}}
}

// lockFor returns the per-VM mutex, creating it on first use. Entries are never reaped: one mutex
// per VM name for the daemon's lifetime is a few dozen bytes, and reaping would reintroduce exactly
// the race this exists to remove.
func (o *vmOps) lockFor(name string) *sync.Mutex {
	o.mu.Lock()
	defer o.mu.Unlock()
	m, ok := o.locks[name]
	if !ok {
		m = &sync.Mutex{}
		o.locks[name] = m
	}
	return m
}

// with runs fn holding the VM's transition lock.
func (o *vmOps) with(name string, fn func() error) error {
	m := o.lockFor(name)
	m.Lock()
	defer m.Unlock()
	return fn()
}

// beginExec marks a command as running in the VM and returns the function that ends it.
//
// The returned func stamps LastActivity at COMPLETION as well. Stamping only at the start makes a
// long command look idle for its whole duration; stamping at the end means the idle clock measures
// idleness rather than command runtime.
func (o *vmOps) beginExec(name string, store *state.Store) func() {
	o.mu.Lock()
	o.inFlight[name]++
	o.mu.Unlock()

	return func() {
		o.mu.Lock()
		o.inFlight[name]--
		if o.inFlight[name] <= 0 {
			delete(o.inFlight, name)
		}
		o.mu.Unlock()

		now := time.Now()
		store.UpdateVM(name, func(v *state.VM) { v.LastActivity = &now })
	}
}

// busy reports whether a command is running in the VM. The sweep must not pause or archive one:
// freezing a VM mid-build is indistinguishable, from inside the guest, from the host hanging.
func (o *vmOps) busy(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inFlight[name] > 0
}
