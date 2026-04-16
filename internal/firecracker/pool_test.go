package firecracker

import (
	"testing"
)

func TestPoolSlotDir(t *testing.T) {
	tests := []struct {
		idx  int
		want string
	}{
		{0, "/opt/mvm/pool/slot0"},
		{1, "/opt/mvm/pool/slot1"},
		{2, "/opt/mvm/pool/slot2"},
	}
	for _, tt := range tests {
		got := poolSlotDir(tt.idx)
		if got != tt.want {
			t.Errorf("poolSlotDir(%d) = %q, want %q", tt.idx, got, tt.want)
		}
	}
}

func TestPoolSocketPath(t *testing.T) {
	tests := []struct {
		idx  int
		want string
	}{
		{0, "/run/mvm/pool0.socket"},
		{1, "/run/mvm/pool1.socket"},
	}
	for _, tt := range tests {
		got := poolSocketPath(tt.idx)
		if got != tt.want {
			t.Errorf("poolSocketPath(%d) = %q, want %q", tt.idx, got, tt.want)
		}
	}
}

func TestPoolSize(t *testing.T) {
	if PoolSize < 1 || PoolSize > 10 {
		t.Errorf("PoolSize = %d, should be 1-10", PoolSize)
	}
}

func TestPoolSocketPathBackwardCompat(t *testing.T) {
	if PoolSocketPath() != poolSocketPath(0) {
		t.Errorf("PoolSocketPath() = %q, want %q", PoolSocketPath(), poolSocketPath(0))
	}
}

// === NEW TESTS ===

func TestPoolSlotDirContainsPoolDir(t *testing.T) {
	for i := 0; i < 3; i++ {
		got := poolSlotDir(i)
		if got[:len(PoolDir())] != PoolDir() {
			t.Errorf("poolSlotDir(%d) = %q, should start with %q", i, got, PoolDir())
		}
	}
}

func TestPoolPidFile(t *testing.T) {
	got := poolPidFile(0)
	want := "/opt/mvm/pool/slot0/pid"
	if got != want {
		t.Errorf("poolPidFile(0) = %q, want %q", got, want)
	}
}

func TestPoolReadyFile(t *testing.T) {
	got := poolReadyFile(0)
	want := "/opt/mvm/pool/slot0/ready"
	if got != want {
		t.Errorf("poolReadyFile(0) = %q, want %q", got, want)
	}
}

// Pool supports multiple slots (multi-slot pool design)
func TestPoolMultiSlot(t *testing.T) {
	if PoolSize < 1 {
		t.Errorf("PoolSize = %d, should be >= 1", PoolSize)
	}
}

func TestPoolSocketPathForSlot(t *testing.T) {
	for i := 0; i < 3; i++ {
		if PoolSocketPathForSlot(i) != poolSocketPath(i) {
			t.Errorf("PoolSocketPathForSlot(%d) != poolSocketPath(%d)", i, i)
		}
	}
}

func TestPoolSizeIsMultiSlot(t *testing.T) {
	if PoolSize != 3 {
		t.Errorf("PoolSize = %d, want 3 (multi-slot for concurrent sessions)", PoolSize)
	}
}

func TestPoolDirDefault(t *testing.T) {
	if PoolDir() != "/opt/mvm/pool" {
		t.Errorf("PoolDir() = %q, want /opt/mvm/pool", PoolDir())
	}
}
