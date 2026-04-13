package state

import (
	"fmt"
	"net"
)

// NetAllocation holds the network configuration for a single microVM.
type NetAllocation struct {
	Index    int
	TAPDev   string
	TAPIP    string // gateway IP on the host (Lima) side
	GuestIP  string // IP inside the microVM
	Subnet   string // CIDR notation
	GuestMAC string
}

// AllocateNet computes network allocation for a given index.
// Each VM gets a /30 subnet from the 172.16.0.0/24 range:
//
//	Index 0: tap0, 172.16.0.1 (gw), 172.16.0.2 (guest)
//	Index 1: tap1, 172.16.0.5 (gw), 172.16.0.6 (guest)
//	Index N: tapN, 172.16.0.(4N+1) (gw), 172.16.0.(4N+2) (guest)
func AllocateNet(index int) NetAllocation {
	base := 4 * index
	gwIP := fmt.Sprintf("172.16.0.%d", base+1)
	guestIP := fmt.Sprintf("172.16.0.%d", base+2)
	subnet := fmt.Sprintf("172.16.0.%d/30", base)

	return NetAllocation{
		Index:    index,
		TAPDev:   fmt.Sprintf("tap%d", index),
		TAPIP:    gwIP,
		GuestIP:  guestIP,
		Subnet:   subnet,
		GuestMAC: ipToMAC(net.ParseIP(guestIP)),
	}
}

// ipToMAC derives a deterministic MAC address from an IP.
// Format: 06:00:AC:10:XX:XX where XX:XX comes from the last two octets.
func ipToMAC(ip net.IP) string {
	ip = ip.To4()
	if ip == nil {
		return "06:00:AC:10:00:02"
	}
	return fmt.Sprintf("06:00:AC:10:%02X:%02X", ip[2], ip[3])
}

// SetupTAPScript returns a shell script that creates a TAP device for a VM.
func SetupTAPScript(alloc NetAllocation) string {
	return fmt.Sprintf(`#!/bin/bash
set -e
sudo ip link del %s 2>/dev/null || true
sudo ip tuntap add dev %s mode tap
sudo ip addr add %s/30 dev %s
sudo ip link set dev %s up
`, alloc.TAPDev, alloc.TAPDev, alloc.TAPIP, alloc.TAPDev, alloc.TAPDev)
}

// DeleteTAPScript returns a shell script that removes a TAP device.
func DeleteTAPScript(tapDev string) string {
	return fmt.Sprintf("sudo ip link del %s 2>/dev/null || true\n", tapDev)
}

// SetupNATScript returns a shell script that configures NAT and IP forwarding.
// Run once during mvm init.
func SetupNATScript() string {
	return `#!/bin/bash
set -e

# Enable IP forwarding
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
sudo mkdir -p /etc/sysctl.d
echo "net.ipv4.ip_forward=1" | sudo tee /etc/sysctl.d/99-mvm.conf >/dev/null

# Detect default network interface
HOST_IFACE=$(ip route list default | awk '{print $5; exit}')
if [ -z "$HOST_IFACE" ]; then
    echo "ERROR: Could not detect default network interface" >&2
    exit 1
fi

# Set up MASQUERADE for microVM traffic
sudo iptables -t nat -C POSTROUTING -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null || \
    sudo iptables -t nat -A POSTROUTING -o "$HOST_IFACE" -j MASQUERADE

# Allow forwarding
sudo iptables -P FORWARD ACCEPT

echo "NAT configured on $HOST_IFACE"
`
}

// NATSystemdServiceScript returns a shell script that installs a helper script
// and systemd service to re-apply NAT rules on Lima VM boot.
func NATSystemdServiceScript() string {
	return `#!/bin/bash
set -e

# Write the NAT setup script
cat <<'SCRIPT' | sudo tee /usr/local/bin/mvm-nat.sh >/dev/null
#!/bin/bash
set -e
sysctl -w net.ipv4.ip_forward=1
HOST_IFACE=$(ip route list default | awk '{print $5; exit}')
if [ -z "$HOST_IFACE" ]; then
    echo "ERROR: Could not detect default network interface" >&2
    exit 1
fi
iptables -t nat -C POSTROUTING -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -o "$HOST_IFACE" -j MASQUERADE
iptables -P FORWARD ACCEPT
echo "NAT configured on $HOST_IFACE"
SCRIPT
sudo chmod +x /usr/local/bin/mvm-nat.sh

# Write the systemd unit
cat <<'UNIT' | sudo tee /etc/systemd/system/mvm-nat.service >/dev/null
[Unit]
Description=MVM NAT forwarding rules
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/mvm-nat.sh

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable mvm-nat.service >/dev/null 2>&1
sudo systemctl start mvm-nat.service
echo "NAT systemd service installed"
`
}
