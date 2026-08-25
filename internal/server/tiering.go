package server

import (
	"context"
	"log"
	"time"

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
				if err := firecracker.Pause(s.executor, vm); err != nil {
					log.Printf("tiering: pause %s: %v", vm.Name, err)
					continue
				}
				s.store.UpdateVM(vm.Name, func(v *state.VM) { v.Status = "paused" })
				log.Printf("tiering: paused %s after %s idle (vCPUs frozen, %dMB still held)", vm.Name, idle.Round(time.Second), vm.MemoryMB)
			}

		case "paused":
			d, ok := parseThreshold(vm.ArchiveAfter)
			if ok && idle >= d {
				if _, err := s.archiveVM(vm); err != nil {
					log.Printf("tiering: archive %s: %v", vm.Name, err)
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
