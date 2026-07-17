package handler

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandleSetupNetwork configures the default route and DNS.
func HandleSetupNetwork(req *protocol.NetworkRequest) *protocol.Response {
	// Add default route
	if req.DefaultGateway != "" {
		cmd := exec.Command("ip", "route", "add", "default", "via", req.DefaultGateway, "dev", "eth0")
		cmd.Run() // ignore error if route already exists
	}

	// Set DNS
	if req.DNS != "" {
		os.WriteFile("/etc/resolv.conf", []byte("nameserver "+req.DNS+"\n"), 0o644)
	}

	return &protocol.Response{Type: protocol.RespOK}
}

// HandleNetInfo reports the guest's actual current network configuration.
// It never shells out — the guest image has no ip/ifconfig binary — it uses
// the Go net package (interface addresses) and /proc/net/route directly.
// This is how the host learns the address DHCP actually handed out on the
// applevz backend (see internal/cli/start.go's post-boot discovery step).
func HandleNetInfo() *protocol.Response {
	info := protocol.NetInfo{
		IP:      InterfaceIPv4("eth0"),
		Gateway: DefaultGatewayIP(),
	}
	data, err := json.Marshal(info)
	if err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}
	return &protocol.Response{Type: protocol.RespOK, Data: data}
}

// InterfaceIPv4 returns the first IPv4 address assigned to the named
// interface, or "" if it has none (or doesn't exist).
func InterfaceIPv4(name string) string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}

// DefaultGatewayIP returns the guest's default-route gateway, or "" if it
// can't be determined. Parses /proc/net/route, where the default route has
// Destination 00000000 and the Gateway is a hex little-endian IPv4.
func DefaultGatewayIP() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return ParseDefaultGateway(string(data))
}

// ParseDefaultGateway extracts the default-route gateway from the contents
// of /proc/net/route. Exported (and split from DefaultGatewayIP) so it's
// testable without a real /proc filesystem.
func ParseDefaultGateway(routeTable string) string {
	for _, line := range strings.Split(routeTable, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" || f[2] == "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%d.%d.%d.%d", byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return ""
}
