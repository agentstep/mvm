package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// === WritePID ===

func TestWritePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	if err := WritePID(path); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse PID: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}
}

func TestWritePIDCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.pid")

	WritePID(path)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("PID file should exist after WritePID")
	}
}

// === ReadPID ===

func TestReadPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")
	os.WriteFile(path, []byte("42"), 0o644)

	pid, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != 42 {
		t.Errorf("PID = %d, want 42", pid)
	}
}

func TestReadPIDNonexistent(t *testing.T) {
	pid, err := ReadPID("/nonexistent/path/test.pid")
	if err != nil {
		t.Fatalf("ReadPID nonexistent should not error: %v", err)
	}
	if pid != 0 {
		t.Errorf("PID = %d, want 0 for nonexistent file", pid)
	}
}

func TestReadPIDInvalidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pid")
	os.WriteFile(path, []byte("not-a-number"), 0o644)

	_, err := ReadPID(path)
	if err == nil {
		t.Error("should error on non-numeric PID file")
	}
}

func TestReadPIDWithWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws.pid")
	os.WriteFile(path, []byte("  123  \n"), 0o644)

	pid, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != 123 {
		t.Errorf("PID = %d, want 123", pid)
	}
}

// === IsRunning ===

func TestIsRunningCurrentProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "running.pid")
	os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)

	running, pid, err := IsRunning(path)
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if !running {
		t.Error("current process should be running")
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}
}

func TestIsRunningNonexistentPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dead.pid")
	// PID 999999999 is extremely unlikely to exist
	os.WriteFile(path, []byte("999999999"), 0o644)

	running, pid, _ := IsRunning(path)
	if running {
		t.Error("PID 999999999 should not be running")
	}
	if pid != 999999999 {
		t.Errorf("PID = %d, want 999999999", pid)
	}
}

func TestIsRunningNoPIDFile(t *testing.T) {
	running, pid, err := IsRunning("/nonexistent/test.pid")
	if err != nil {
		t.Fatalf("IsRunning nonexistent: %v", err)
	}
	if running {
		t.Error("should not be running with no PID file")
	}
	if pid != 0 {
		t.Errorf("PID = %d, want 0", pid)
	}
}

// === RemovePID ===

func TestRemovePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove.pid")
	os.WriteFile(path, []byte("1234"), 0o644)

	if err := RemovePID(path); err != nil {
		t.Fatalf("RemovePID: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("PID file should be removed")
	}
}

func TestRemovePIDNonexistent(t *testing.T) {
	err := RemovePID("/nonexistent/path/test.pid")
	if err == nil {
		t.Error("RemovePID should error on nonexistent file")
	}
}

// === CheckNotRunning ===

func TestCheckNotRunningNoPIDFile(t *testing.T) {
	err := CheckNotRunning("/nonexistent/path/test.pid")
	if err != nil {
		t.Errorf("should succeed with no PID file: %v", err)
	}
}

func TestCheckNotRunningDeadProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dead.pid")
	os.WriteFile(path, []byte("999999999"), 0o644)

	err := CheckNotRunning(path)
	if err != nil {
		t.Errorf("should succeed when process is dead: %v", err)
	}

	// Stale PID file should be cleaned up
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale PID file should be removed")
	}
}

func TestCheckNotRunningAliveProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alive.pid")
	// Use current process PID (which is definitely running)
	os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)

	err := CheckNotRunning(path)
	if err == nil {
		t.Error("should error when process is running")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error should mention 'already running', got: %v", err)
	}
}

// === WritePID + ReadPID roundtrip ===

func TestWriteReadPIDRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roundtrip.pid")

	if err := WritePID(path); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	pid, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}

	if pid != os.Getpid() {
		t.Errorf("PID roundtrip: got %d, want %d", pid, os.Getpid())
	}
}
