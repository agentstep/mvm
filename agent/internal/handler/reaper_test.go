package handler

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"testing"
)

// === ExitCodeFrom ===

func TestExitCodeFromNil(t *testing.T) {
	code, diag := ExitCodeFrom(nil)
	if code != 0 || diag != "" {
		t.Errorf("ExitCodeFrom(nil) = (%d, %q), want (0, \"\")", code, diag)
	}
}

func TestExitCodeFromRealExit(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 3").Run()
	code, diag := ExitCodeFrom(err)
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if diag != "" {
		t.Errorf("diagnostic = %q, want empty for a real exit", diag)
	}
}

// TestExitCodeFromECHILDIsNotSuccess is the regression guard on the bug this
// package existed with: exec_stream and exec_pty assigned exitCode only inside
// the *exec.ExitError branch, so any other Wait() error left it at 0 and the
// exit frame reported SUCCESS for a command whose status was never collected.
//
// ECHILD is the case that matters, because it is what Wait() returns once a
// reaper has already collected the child — i.e. it goes from rare to routine
// the moment the agent starts reaping.
func TestExitCodeFromECHILDIsNotSuccess(t *testing.T) {
	code, diag := ExitCodeFrom(syscall.ECHILD)
	if code == 0 {
		t.Fatal("ECHILD reported as exit 0 — a failed command would look successful")
	}
	if code != ExitWaitFailed {
		t.Errorf("code = %d, want ExitWaitFailed (%d)", code, ExitWaitFailed)
	}
	if diag == "" {
		t.Error("ECHILD must carry a diagnostic so the cause is visible")
	}
}

func TestExitCodeFromWrappedECHILD(t *testing.T) {
	// Wait() errors arrive wrapped in practice; errors.Is must still match.
	code, _ := ExitCodeFrom(&exec.Error{Name: "sh", Err: syscall.ECHILD})
	if code != ExitWaitFailed {
		t.Errorf("wrapped ECHILD gave %d, want %d", code, ExitWaitFailed)
	}
}

func TestExitCodeFromArbitraryErrorIsNotSuccess(t *testing.T) {
	code, diag := ExitCodeFrom(errors.New("something else went wrong"))
	if code == 0 {
		t.Fatal("arbitrary wait error reported as exit 0")
	}
	if diag == "" {
		t.Error("arbitrary wait error must carry a diagnostic")
	}
}

// === statusRegistry ===

func TestStatusRegistryDeliversToWaiter(t *testing.T) {
	r := newStatusRegistry()
	ch := r.register(4242)
	r.deliver(4242, 7)

	select {
	case got := <-ch:
		if got != 7 {
			t.Errorf("status = %d, want 7", got)
		}
	default:
		t.Fatal("registered waiter did not receive the status")
	}
}

// TestStatusRegistryIgnoresUnregisteredPid covers the orphan case: the reaper
// collects pids nobody is waiting on (every `mvm exec -d` detached job
// reparents to PID 1), and that must not block or panic.
func TestStatusRegistryIgnoresUnregisteredPid(t *testing.T) {
	r := newStatusRegistry()
	r.deliver(9999, 0) // no waiter; must not create one
	if n := r.len(); n != 0 {
		t.Errorf("registry retained %d waiters after delivering to an orphan pid", n)
	}
}

// TestStatusRegistryRegisterAfterReap is the deadlock guard. A child can exit
// between Start() returning and its pid being registered; the reaper then
// delivers to nobody. Without the pending cache the session would block forever
// on a channel nothing will ever send to.
func TestStatusRegistryRegisterAfterReap(t *testing.T) {
	r := newStatusRegistry()
	r.deliver(4242, 42) // reaped first...
	ch := r.register(4242)

	select {
	case got := <-ch:
		if got != 42 {
			t.Errorf("status = %d, want 42", got)
		}
	default:
		t.Fatal("registering after the reap did not recover the status — a session would hang here")
	}
	if r.pendingLen() != 0 {
		t.Errorf("claimed status left %d entries in the pending cache", r.pendingLen())
	}
}

// TestStatusRegistryPendingIsBounded pins that unclaimed orphan statuses cannot
// grow without limit. Every detached job produces one that is never claimed, so
// an uncapped cache would leak for the life of the VM.
func TestStatusRegistryPendingIsBounded(t *testing.T) {
	r := newStatusRegistry()
	for pid := 1; pid <= maxPendingStatuses*3; pid++ {
		r.deliver(pid, 0)
	}
	if got := r.pendingLen(); got > maxPendingStatuses+1 {
		t.Errorf("pending cache grew to %d, want <= %d", got, maxPendingStatuses+1)
	}
}

// TestStatusRegistryDeliverIsNonBlocking pins that a reaper delivering to a
// waiter that has gone away cannot wedge. If deliver() blocked, one abandoned
// session would stall reaping for the whole namespace and zombies would pile
// up without bound.
func TestStatusRegistryDeliverIsNonBlocking(t *testing.T) {
	r := newStatusRegistry()
	r.register(1234)
	r.deliver(1234, 1) // fills the buffered slot
	r.deliver(1234, 2) // waiter already satisfied and gone; must not block

	done := make(chan struct{})
	go func() { r.deliver(1234, 3); close(done) }()
	select {
	case <-done:
	default:
		// Give the goroutine a moment only if it hasn't already finished.
		<-done
	}
}

func TestStatusRegistryUnregisterRemoves(t *testing.T) {
	r := newStatusRegistry()
	r.register(555)
	if r.len() != 1 {
		t.Fatalf("len = %d after register, want 1", r.len())
	}
	r.unregister(555)
	if r.len() != 0 {
		t.Errorf("len = %d after unregister, want 0", r.len())
	}
}

// TestStatusRegistryConcurrent is the load case the exit-code bug hid behind:
// many sessions registering and reaping at once. Run with -race.
func TestStatusRegistryConcurrent(t *testing.T) {
	r := newStatusRegistry()
	const n = 200
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(pid int) {
			defer wg.Done()
			ch := r.register(pid)
			r.deliver(pid, pid%256)
			<-ch
			r.unregister(pid)
		}(i)
	}
	wg.Wait()
	if r.len() != 0 {
		t.Errorf("registry leaked %d entries", r.len())
	}
}
