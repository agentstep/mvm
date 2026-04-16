package server

import (
	"runtime"
	"testing"
)

// === Constants ===

func TestDaemonSocketPathConstant(t *testing.T) {
	if DaemonSocketPath != "/run/mvm/daemon.sock" {
		t.Errorf("DaemonSocketPath = %q, want /run/mvm/daemon.sock", DaemonSocketPath)
	}
}

func TestDaemonTCPPortConstant(t *testing.T) {
	if DaemonTCPPort != 19876 {
		t.Errorf("DaemonTCPPort = %d, want 19876", DaemonTCPPort)
	}
}

// === DefaultSocketPath ===

func TestDefaultSocketPathNotEmpty(t *testing.T) {
	path := DefaultSocketPath()
	if path == "" {
		t.Error("DefaultSocketPath should not be empty")
	}
}

func TestDefaultSocketPathPlatform(t *testing.T) {
	path := DefaultSocketPath()
	if runtime.GOOS == "linux" {
		if path != DaemonSocketPath {
			t.Errorf("on Linux, DefaultSocketPath = %q, want %q", path, DaemonSocketPath)
		}
	} else {
		// On macOS, should not return the Linux path
		// (unless Lima-forwarded socket exists)
		if path == DaemonSocketPath {
			t.Log("DefaultSocketPath returned Linux path on non-Linux (Lima forward exists)")
		}
	}
}

// === DefaultPIDPath ===

func TestDefaultPIDPathNotEmpty(t *testing.T) {
	path := DefaultPIDPath()
	if path == "" {
		t.Error("DefaultPIDPath should not be empty")
	}
}

func TestDefaultPIDPathPlatform(t *testing.T) {
	path := DefaultPIDPath()
	if runtime.GOOS == "linux" {
		if path != "/run/mvm/daemon.pid" {
			t.Errorf("on Linux, DefaultPIDPath = %q, want /run/mvm/daemon.pid", path)
		}
	}
}

// === DefaultStatePath ===

func TestDefaultStatePathNotEmpty(t *testing.T) {
	path := DefaultStatePath()
	if path == "" {
		t.Error("DefaultStatePath should not be empty")
	}
}

func TestDefaultStatePathContainsStateJSON(t *testing.T) {
	path := DefaultStatePath()
	if len(path) < 10 {
		t.Errorf("DefaultStatePath too short: %q", path)
	}
	// Should end with state.json
	suffix := "state.json"
	if path[len(path)-len(suffix):] != suffix {
		t.Errorf("DefaultStatePath should end with state.json, got: %q", path)
	}
}

// === IsLinux ===

func TestIsLinux(t *testing.T) {
	expected := runtime.GOOS == "linux"
	if IsLinux() != expected {
		t.Errorf("IsLinux() = %v, want %v (GOOS=%s)", IsLinux(), expected, runtime.GOOS)
	}
}

// === Config validation ===

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	if cfg.SocketPath != "" {
		t.Error("SocketPath should default to empty (filled by New)")
	}
	if cfg.PIDPath != "" {
		t.Error("PIDPath should default to empty (filled by New)")
	}
}

func TestConfigWithCustomPaths(t *testing.T) {
	cfg := Config{
		SocketPath: "/custom/path.sock",
		PIDPath:    "/custom/path.pid",
	}
	if cfg.SocketPath != "/custom/path.sock" {
		t.Errorf("SocketPath = %q", cfg.SocketPath)
	}
	if cfg.PIDPath != "/custom/path.pid" {
		t.Errorf("PIDPath = %q", cfg.PIDPath)
	}
}
