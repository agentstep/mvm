package vm

import (
	"testing"
)

func TestNewAppleVZBackend(t *testing.T) {
	b := NewAppleVZBackend("/tmp/test-mvm")
	if b.Name() != "applevz" {
		t.Errorf("Name = %q, want applevz", b.Name())
	}
}

func TestAppleVZIsAvailableWithoutBinary(t *testing.T) {
	b := &AppleVZBackend{
		binary:   "/nonexistent/mvm-vz",
		dataDir:  "/tmp",
		cacheDir: "/tmp",
	}
	if b.IsAvailable() {
		t.Error("should not be available with nonexistent binary")
	}
}

func TestAppleVZIsRunningWithBadPID(t *testing.T) {
	b := NewAppleVZBackend("/tmp")
	if b.IsRunning(0) {
		t.Error("PID 0 should not be running")
	}
	if b.IsRunning(-1) {
		t.Error("PID -1 should not be running")
	}
}

// === NEW TESTS ===

func TestAppleVZBackendName(t *testing.T) {
	b := NewAppleVZBackend("/tmp/mvm")
	if b.Name() != "applevz" {
		t.Errorf("Name() = %q, want applevz", b.Name())
	}
}

func TestAppleVZBackendDataDir(t *testing.T) {
	b := NewAppleVZBackend("/home/user/.mvm")
	if b.dataDir != "/home/user/.mvm" {
		t.Errorf("dataDir = %q, want /home/user/.mvm", b.dataDir)
	}
	if b.cacheDir != "/home/user/.mvm/cache" {
		t.Errorf("cacheDir = %q, want /home/user/.mvm/cache", b.cacheDir)
	}
}

func TestAppleVZIsRunningNegativePID(t *testing.T) {
	b := NewAppleVZBackend("/tmp")
	if b.IsRunning(-100) {
		t.Error("negative PID should not be running")
	}
}

func TestAppleVZIsRunningWithHighPID(t *testing.T) {
	b := NewAppleVZBackend("/tmp")
	// Very high PID that definitely doesn't exist
	if b.IsRunning(999999999) {
		t.Error("nonexistent PID should not be running")
	}
}
