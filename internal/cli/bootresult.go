package cli

import "time"

// outputMode controls what a start prints. Lets the bench harness drive the
// real start path in-process and collect the BootResult without any output.
type outputMode int

const (
	outHuman outputMode = iota // human-readable summary on stdout
	outJSON                    // a single JSON BootResult on stdout
	outQuiet                   // nothing; caller consumes the returned BootResult
)

// BootPath identifies which path a VM start took. Reported on every start so
// warm-pool numbers are never silently published as a cold-boot headline.
type BootPath string

const (
	BootCold     BootPath = "cold_boot"        // booted a fresh rootfs
	BootRestore  BootPath = "snapshot_restore" // restored from a saved memory+disk checkpoint
	BootPoolClaim BootPath = "pool_claim"      // claimed a pre-booted warm VM (reserved; no VZ pool yet)
)

// BootResult is the structured outcome of a start, including a per-phase timing
// breakdown. Emitted as JSON with `--json`; also the shape the SDKs should
// eventually return.
type BootResult struct {
	Name       string        `json:"name"`
	Backend    string        `json:"backend"`
	BootPath   BootPath      `json:"boot_path"`
	GuestIP    string        `json:"guest_ip,omitempty"`
	AgentReady bool          `json:"agent_ready"`
	TotalMs    float64       `json:"total_ms"`
	Phases     []PhaseTiming `json:"phases"`
}

// PhaseTiming is one labelled segment of a start.
type PhaseTiming struct {
	Name string  `json:"name"`
	Ms   float64 `json:"ms"`
}

// phaseTimer accumulates labelled phase durations against a moving cursor.
type phaseTimer struct {
	start  time.Time
	cursor time.Time
	phases []PhaseTiming
}

func newPhaseTimer() *phaseTimer {
	now := time.Now()
	return &phaseTimer{start: now, cursor: now}
}

// mark records the elapsed time since the last mark under the given name.
func (t *phaseTimer) mark(name string) {
	now := time.Now()
	t.phases = append(t.phases, PhaseTiming{Name: name, Ms: msSince(t.cursor, now)})
	t.cursor = now
}

func (t *phaseTimer) totalMs() float64 { return msSince(t.start, time.Now()) }

func msSince(from, to time.Time) float64 {
	return float64(to.Sub(from).Microseconds()) / 1000.0
}
