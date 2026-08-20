package container

import (
	"testing"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// TestRouteInsideOnlyUserCode pins which handlers cross into the container.
//
// Getting this wrong is not a style issue. poweroff in particular MUST stay
// outer: reboot(2) called from a non-initial PID namespace terminates the
// namespace rather than the machine, so `mvm stop` would report success while
// merely killing the container and leaving the VM running.
func TestRouteInsideOnlyUserCode(t *testing.T) {
	inside := []string{
		protocol.ReqExec,
		protocol.ReqExecStream,
		protocol.ReqExecPty,
		protocol.ReqWriteFile,
		protocol.ReqReadFile,
	}
	for _, rt := range inside {
		if !RouteInside(rt) {
			t.Errorf("RouteInside(%q) = false, want true — this is user code", rt)
		}
	}

	outside := []string{
		protocol.ReqPing,
		protocol.ReqPoweroff,
		protocol.ReqTCPForward,
		protocol.ReqSetupNet,
		protocol.ReqNetInfo,
		protocol.ReqMount,
	}
	for _, rt := range outside {
		if RouteInside(rt) {
			t.Errorf("RouteInside(%q) = true, want false", rt)
		}
	}
}

// TestRouteUnknownStaysOutside is a fail-safe: an unrecognised verb must not be
// forwarded into the container, where the inner init may not know it either and
// the connection would be consumed with no reply.
func TestRouteUnknownStaysOutside(t *testing.T) {
	for _, rt := range []string{"", "definitely_not_a_verb", "exec_"} {
		if RouteInside(rt) {
			t.Errorf("RouteInside(%q) = true, want false for an unknown verb", rt)
		}
	}
}

// TestPoweroffNeverRoutesInside is called out separately because it is the one
// with a silent, actively misleading failure mode.
func TestPoweroffNeverRoutesInside(t *testing.T) {
	if RouteInside(protocol.ReqPoweroff) {
		t.Fatal("poweroff must never route inside: reboot(2) in a non-initial " +
			"PID namespace kills the namespace, not the machine, so `mvm stop` " +
			"would report success while the VM kept running")
	}
}

// TestTCPForwardNeverRoutesInside — the netns is shared, so dialling 127.0.0.1
// from the outer namespace reaches inner services identically. Keeping it outer
// also means the raw unframed relay never has to cross the fd-passing boundary.
func TestTCPForwardNeverRoutesInside(t *testing.T) {
	if RouteInside(protocol.ReqTCPForward) {
		t.Fatal("tcp_forward must stay outer — shared netns makes it equivalent, " +
			"and its raw relay would complicate the transport for no gain")
	}
}
