package handler

import (
	"net"
	"testing"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// TestParseDefaultGatewayRealFixture uses a real /proc/net/route capture from
// a booted applevz guest (ip=dhcp against Apple's VZNATNetworkDeviceAttachment)
// to lock in the parsing behavior this package's HandleNetInfo depends on.
func TestParseDefaultGatewayRealFixture(t *testing.T) {
	const routeTable = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT" +
		"\neth0\t00000000\t0141A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0" +
		"\neth0\t0041A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"

	got := ParseDefaultGateway(routeTable)
	want := "192.168.65.1"
	if got != want {
		t.Fatalf("ParseDefaultGateway() = %q, want %q", got, want)
	}
}

func TestParseDefaultGatewayNoDefaultRoute(t *testing.T) {
	// Only a connected (non-default) route — Destination is non-zero.
	const routeTable = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT" +
		"\neth0\t0041A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"

	if got := ParseDefaultGateway(routeTable); got != "" {
		t.Fatalf("ParseDefaultGateway() = %q, want empty (no default route)", got)
	}
}

func TestParseDefaultGatewayZeroGatewayIgnored(t *testing.T) {
	// A default-destination row whose gateway is also 0.0.0.0 is a
	// directly-connected route, not an actual gateway — must be skipped.
	const routeTable = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT" +
		"\neth0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n"

	if got := ParseDefaultGateway(routeTable); got != "" {
		t.Fatalf("ParseDefaultGateway() = %q, want empty (zero gateway)", got)
	}
}

func TestParseDefaultGatewayEmpty(t *testing.T) {
	if got := ParseDefaultGateway(""); got != "" {
		t.Fatalf("ParseDefaultGateway(\"\") = %q, want empty", got)
	}
}

// TestInterfaceIPv4Loopback exercises the real net package path (no /proc
// fixture) against this machine's loopback interface — proves the
// interface-address lookup itself works without needing eth0 or a real
// guest. The loopback interface's name differs by OS ("lo" on the Linux
// guest this ships on, "lo0" when this test runs on a macOS dev machine),
// so it's discovered rather than hard-coded.
func TestInterfaceIPv4Loopback(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces(): %v", err)
	}
	var loopback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			loopback = iface.Name
			break
		}
	}
	if loopback == "" {
		t.Skip("no loopback interface found on this host")
	}

	got := InterfaceIPv4(loopback)
	if got != "127.0.0.1" {
		t.Fatalf("InterfaceIPv4(%q) = %q, want \"127.0.0.1\"", loopback, got)
	}
}

func TestInterfaceIPv4Missing(t *testing.T) {
	if got := InterfaceIPv4("does-not-exist0"); got != "" {
		t.Fatalf("InterfaceIPv4() = %q, want empty for a missing interface", got)
	}
}

// TestHandleNetInfoShape confirms the response carries valid JSON with the
// expected fields, independent of what this host's actual network looks
// like (CI/dev machines aren't guests, so IP/Gateway may be empty here).
func TestHandleNetInfoShape(t *testing.T) {
	resp := HandleNetInfo()
	if resp.Type != protocol.RespOK {
		t.Fatalf("HandleNetInfo().Type = %q, want %q", resp.Type, protocol.RespOK)
	}
	if len(resp.Data) == 0 {
		t.Fatal("HandleNetInfo().Data is empty, want JSON-encoded NetInfo")
	}
}
