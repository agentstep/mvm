package menubar

import (
	"fmt"
	"sort"

	"github.com/agentstep/mvm/internal/server"
	"github.com/caseymrm/menuet"
)

func (a *App) MenuItems() []menuet.MenuItem {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var items []menuet.MenuItem

	// VM list
	if len(a.vms) == 0 {
		items = append(items, menuet.MenuItem{Text: "No VMs"})
	} else {
		sorted := make([]server.VMResponse, len(a.vms))
		copy(sorted, a.vms)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

		for _, vm := range sorted {
			items = append(items, a.vmMenuItem(vm))
		}
	}

	items = append(items, menuet.MenuItem{Type: menuet.Separator})

	if !a.daemonRunning {
		items = append(items, menuet.MenuItem{Text: "Not connected — run: mvm init"})
	}

	// Pool status
	if a.poolTotal > 0 {
		items = append(items, menuet.MenuItem{
			Text: fmt.Sprintf("Pool: %d/%d warm", a.poolReady, a.poolTotal),
		})
	}

	items = append(items, menuet.MenuItem{Type: menuet.Separator})

	items = append(items, menuet.MenuItem{
		Text:    "Open Terminal",
		Clicked: func() { go a.openTerminal() },
	})

	return items
}

func (a *App) vmMenuItem(vm server.VMResponse) menuet.MenuItem {
	icon := statusIcon(vm.Status)
	text := fmt.Sprintf("%s %s", icon, vm.Name)
	if vm.GuestIP != "" && vm.Status == "running" {
		text = fmt.Sprintf("%s %s (%s)", icon, vm.Name, vm.GuestIP)
	}

	return menuet.MenuItem{
		Text: text,
		Children: func() []menuet.MenuItem {
			var sub []menuet.MenuItem
			sub = append(sub, menuet.MenuItem{Text: fmt.Sprintf("Status: %s", vm.Status)})
			if vm.GuestIP != "" {
				sub = append(sub, menuet.MenuItem{Text: fmt.Sprintf("IP: %s", vm.GuestIP)})
			}
			name := vm.Name
			if vm.Status == "running" {
				sub = append(sub, menuet.MenuItem{Text: "SSH", Clicked: func() { go a.sshToVM(name) }})
				sub = append(sub, menuet.MenuItem{Text: "Stop", Clicked: func() { go a.stopVM(name) }})
			}
			if vm.Status == "stopped" {
				sub = append(sub, menuet.MenuItem{Text: "Delete", Clicked: func() { go a.deleteVM(name) }})
			}
			return sub
		},
	}
}

func statusIcon(status string) string {
	switch status {
	case "running":
		return "\u25CF" // filled circle
	case "paused":
		return "\u25CB" // open circle
	case "stopped":
		return "\u25AA" // small square
	default:
		return "?"
	}
}
