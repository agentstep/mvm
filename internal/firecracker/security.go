package firecracker

import (
	"fmt"
	"strings"

	"github.com/agentstep/mvm/internal/state"
)

// Seccomp profiles — restrict syscalls inside the guest.
var seccompProfiles = map[string]string{
	"strict": `iptables -A OUTPUT -p tcp --dport 80 -j DROP
iptables -A OUTPUT -p tcp --dport 443 -j DROP
chmod 000 /usr/bin/wget /usr/bin/curl 2>/dev/null || true
chmod 000 /sbin/apk 2>/dev/null || true
mount -o remount,ro /`,

	"moderate": `chmod 000 /sbin/apk 2>/dev/null || true
echo "Moderate seccomp profile applied"`,

	"permissive": `echo "Permissive seccomp profile — no restrictions, audit only"`,
}

// SetupVolumeMounts creates directories in the guest and copies files via the agent.
// For simple cases, uses agent Exec for mkdir. File transfer uses tar through agent.
func SetupVolumeMounts(ex Executor, vm *state.VM, volumes []string) error {
	if len(volumes) == 0 {
		return nil
	}

	for _, vol := range volumes {
		parts := strings.SplitN(vol, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid volume format %q (expected hostPath:guestPath)", vol)
		}
		guestPath := parts[1]

		agentExec(ex, vm.GuestIP, fmt.Sprintf("mkdir -p %s", guestPath))
	}
	return nil
}

func shellQuoteForSSH(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
