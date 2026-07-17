package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

func TestMergeVMResponses(t *testing.T) {
	local := []server.VMResponse{
		{Name: "vz-a", Backend: "applevz", GuestIP: "192.168.65.2"},
	}
	daemon := []server.VMResponse{
		{Name: "fc-a", Backend: "firecracker", GuestIP: "172.16.0.2"},
	}

	t.Run("both empty", func(t *testing.T) {
		if got := mergeVMResponses(nil, nil); len(got) != 0 {
			t.Fatalf("mergeVMResponses(nil, nil) = %v, want empty", got)
		}
	})

	t.Run("local only", func(t *testing.T) {
		got := mergeVMResponses(local, nil)
		if len(got) != 1 || got[0].Name != "vz-a" {
			t.Fatalf("mergeVMResponses(local, nil) = %v, want [vz-a]", got)
		}
	})

	t.Run("daemon only", func(t *testing.T) {
		got := mergeVMResponses(nil, daemon)
		if len(got) != 1 || got[0].Name != "fc-a" {
			t.Fatalf("mergeVMResponses(nil, daemon) = %v, want [fc-a]", got)
		}
	})

	t.Run("both, combined", func(t *testing.T) {
		got := mergeVMResponses(local, daemon)
		if len(got) != 2 {
			t.Fatalf("mergeVMResponses(local, daemon) len = %d, want 2", len(got))
		}
		names := map[string]bool{}
		for _, vm := range got {
			names[vm.Name] = true
		}
		if !names["vz-a"] || !names["fc-a"] {
			t.Fatalf("mergeVMResponses(local, daemon) = %v, want both vz-a and fc-a", got)
		}
	})

	t.Run("name collision prefers local (applevz is the ground truth for its own names)", func(t *testing.T) {
		collidingDaemon := []server.VMResponse{
			{Name: "vz-a", Backend: "firecracker", GuestIP: "172.16.0.9"},
		}
		got := mergeVMResponses(local, collidingDaemon)
		if len(got) != 1 {
			t.Fatalf("mergeVMResponses with colliding name len = %d, want 1 (deduped)", len(got))
		}
		if got[0].Backend != "applevz" || got[0].GuestIP != "192.168.65.2" {
			t.Fatalf("mergeVMResponses collision = %+v, want the local applevz entry to win", got[0])
		}
	})
}

func TestLocalApplevzVMsFiltersBackend(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	now := time.Now()
	if err := store.AddVM(&state.VM{Name: "vz-1", Backend: "applevz", GuestIP: "192.168.65.2", CreatedAt: now}); err != nil {
		t.Fatalf("AddVM(vz-1): %v", err)
	}
	if err := store.AddVM(&state.VM{Name: "fc-1", Backend: "firecracker", GuestIP: "172.16.0.2", CreatedAt: now}); err != nil {
		t.Fatalf("AddVM(fc-1): %v", err)
	}

	got, err := localApplevzVMs(store)
	if err != nil {
		t.Fatalf("localApplevzVMs: %v", err)
	}
	if len(got) != 1 || got[0].Name != "vz-1" {
		t.Fatalf("localApplevzVMs = %v, want only vz-1 (firecracker VMs are the daemon's to report)", got)
	}
	if got[0].GuestIP != "192.168.65.2" {
		t.Fatalf("localApplevzVMs GuestIP = %q, want the DHCP-discovered address", got[0].GuestIP)
	}
}
