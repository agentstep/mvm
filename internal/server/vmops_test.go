package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

func opsStore(t *testing.T) *state.Store {
	t.Helper()
	return state.NewStore(filepath.Join(t.TempDir(), "state.json"))
}

// A VM with a command running in it must never be tiered.
//
// LastActivity is stamped when an exec starts and exec allows five minutes, so with an idle_timeout
// below that the sweep would see an "idle" VM and freeze its vCPUs mid-build. From inside the guest
// that is indistinguishable from the host hanging, and the command's own timeout is what eventually
// surfaces it — as a failure the caller cannot explain.
func TestBusyVMIsNotIdle(t *testing.T) {
	ops := newVMOps()
	store := opsStore(t)

	if ops.busy("vm1") {
		t.Fatal("a VM with no exec should not be busy")
	}

	done := ops.beginExec("vm1", store)
	if !ops.busy("vm1") {
		t.Fatal("a VM with an exec in flight must be busy — the sweep would pause a running command")
	}

	// Concurrent execs: the VM stays busy until the LAST one finishes.
	done2 := ops.beginExec("vm1", store)
	done()
	if !ops.busy("vm1") {
		t.Fatal("one exec finishing marked the VM idle while another was still running")
	}
	done2()
	if ops.busy("vm1") {
		t.Fatal("VM still busy after every exec finished — it would never tier again")
	}
}

// The completion stamp is the point: stamping only at exec start makes a long command look idle for
// its entire duration, so the VM is a candidate for archiving the moment it finishes — or during,
// if nothing else guards it.
func TestExecCompletionStampsActivity(t *testing.T) {
	ops := newVMOps()
	store := opsStore(t)

	old := time.Now().Add(-time.Hour)
	if err := store.AddVM(&state.VM{Name: "vm1", Status: "running", CreatedAt: old, LastActivity: &old}); err != nil {
		t.Fatalf("add vm: %v", err)
	}

	done := ops.beginExec("vm1", store)
	done()

	vm, err := store.GetVM("vm1")
	if err != nil {
		t.Fatalf("get vm: %v", err)
	}
	if vm.LastActivity == nil || !vm.LastActivity.After(old) {
		t.Fatalf("LastActivity = %v, want a fresh stamp — an hour-old one makes a just-finished command look idle", vm.LastActivity)
	}
}

// Transitions on one VM serialise; transitions on different VMs do not.
//
// A five-minute exec holding a global lock would stall the sweep for every other VM on the box, so
// the per-VM granularity is load-bearing, not incidental.
func TestVMOpsSerialisesPerVMNotGlobally(t *testing.T) {
	ops := newVMOps()

	var mu sync.Mutex
	overlapped := false
	inside := map[string]bool{}

	enter := func(name string) {
		mu.Lock()
		if inside[name] {
			overlapped = true
		}
		inside[name] = true
		mu.Unlock()
	}
	leave := func(name string) {
		mu.Lock()
		inside[name] = false
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		for _, name := range []string{"a", "b", "c"} {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				_ = ops.with(n, func() error {
					enter(n)
					time.Sleep(time.Microsecond)
					leave(n)
					return nil
				})
			}(name)
		}
	}
	wg.Wait()

	if overlapped {
		t.Fatal("two transitions ran concurrently on the same VM — archive and restore could interleave")
	}

	// Different VMs must not block each other: two locks held at once is the whole point.
	first := ops.lockFor("a")
	second := ops.lockFor("b")
	first.Lock()
	acquired := make(chan struct{})
	go func() { second.Lock(); close(acquired); second.Unlock() }()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("locking VM a blocked VM b — one long exec would stall the sweep for the whole box")
	}
	first.Unlock()
}

// beginExec/busy are called from the sweep goroutine and from every exec handler at once.
func TestVMOpsIsRaceFree(t *testing.T) {
	ops := newVMOps()
	store := opsStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); ops.beginExec("vm1", store)() }()
		go func() { defer wg.Done(); _ = ops.busy("vm1") }()
	}
	wg.Wait()

	if ops.busy("vm1") {
		t.Fatal("in-flight count did not return to zero — the VM would never tier again")
	}
}

// The sweep must consult busy, and must do its transitions under the per-VM lock. Asserting on the
// source because driving a real pause needs Firecracker.
func TestSweepRespectsInFlightExecs(t *testing.T) {
	src, err := os.ReadFile("tiering.go")
	if err != nil {
		t.Fatalf("read tiering.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "s.ops.busy(vm.Name)") {
		t.Error("the sweep never checks whether a command is running — it would pause a VM mid-build")
	}
	if strings.Count(body, "s.ops.with(vm.Name") < 2 {
		t.Error("both the pause and the archive tier must transition under the VM's lock, or they can interleave with an exec's restore")
	}
	// The re-check inside the lock is what closes the window between the outer check and the
	// transition.
	if strings.Count(body, "s.ops.busy(vm.Name)") < 3 {
		t.Error("busy must be re-checked inside each locked transition, not only once before them")
	}
}

// Exec must wake the VM and mark itself in flight inside ONE critical section. Doing them
// separately leaves a window in which the sweep re-freezes a VM the handler just woke.
func TestExecWakesAndMarksAtomically(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	body := string(src)

	lock := strings.Index(body, "if lerr := s.ops.with(name, func() error {")
	if lock == -1 {
		t.Fatal("exec does not wake the VM under its transition lock")
	}
	begin := strings.Index(body[lock:], "s.ops.beginExec(name, s.store)")
	if begin == -1 {
		t.Fatal("exec never marks itself in flight — the sweep could pause the command it just started")
	}
	restore := strings.Index(body[lock:], "s.restoreArchivedVM(current)")
	if restore == -1 || restore > begin {
		t.Error("the archived-VM restore must happen inside the same locked section as the wake")
	}
}
