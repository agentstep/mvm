package vm

import (
	"os"
	"path/filepath"
	"strings"
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

// Regression guard: IsRunning previously used process.Signal(nil), which
// always errored, so it reported every process — including this live one —
// as not running. The current process must report as running.
func TestAppleVZIsRunningCurrentProcess(t *testing.T) {
	b := NewAppleVZBackend("/tmp")
	if !b.IsRunning(os.Getpid()) {
		t.Error("the current (alive) process must report as running")
	}
}

func TestAppleVZIPCSocketPath(t *testing.T) {
	b := NewAppleVZBackend("/home/me/.mvm")
	got := b.IPCSocketPath("foo")
	want := "/home/me/.mvm/run/vz-foo.sock"
	if got != want {
		t.Errorf("IPCSocketPath = %q, want %q", got, want)
	}
}

func TestAppleVZAgentClientNotNil(t *testing.T) {
	b := NewAppleVZBackend("/home/me/.mvm")
	c := b.AgentClient("foo")
	if c == nil {
		t.Fatal("AgentClient returned nil")
	}
}

func TestAppleVZHelperClientNotNil(t *testing.T) {
	b := NewAppleVZBackend("/home/me/.mvm")
	c := b.HelperClient("foo")
	if c == nil {
		t.Fatal("HelperClient returned nil")
	}
}

// === Bug 4 regression tests: mvm-vz stderr must not be inherited from this
// process's own os.Stderr (which can be a caller's pipe that a fd-holding
// detached helper would keep from ever reaching EOF) — see the cmd.Stderr
// comment in StartVM. It's captured to a per-VM log file instead, and
// startup failures fold that file's content back into the returned error so
// error-surfacing isn't lost.

func TestHelperStderrTailMissingFile(t *testing.T) {
	if got := helperStderrTail(filepath.Join(t.TempDir(), "nope.log")); got != "" {
		t.Errorf("helperStderrTail(missing) = %q, want \"\"", got)
	}
}

func TestHelperStderrTailTrimsAndBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	if err := os.WriteFile(path, []byte("  hello world  \n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := helperStderrTail(path); got != "hello world" {
		t.Errorf("helperStderrTail = %q, want %q", got, "hello world")
	}

	// Bound to the last maxHelperStderrTail bytes so a runaway diagnostic
	// stream can't bloat every subsequent error message.
	big := strings.Repeat("x", maxHelperStderrTail+100) + "TAIL-MARKER"
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	got := helperStderrTail(path)
	if len(got) != maxHelperStderrTail {
		t.Fatalf("helperStderrTail len = %d, want %d", len(got), maxHelperStderrTail)
	}
	if !strings.HasSuffix(got, "TAIL-MARKER") {
		t.Fatalf("helperStderrTail = %q, want it to end with TAIL-MARKER", got)
	}
}

func TestWithHelperStderrAppendsWhenPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	if err := os.WriteFile(path, []byte("disk image not found"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	base := os.ErrNotExist
	got := withHelperStderr(base, path)
	if !strings.Contains(got.Error(), "disk image not found") {
		t.Errorf("withHelperStderr = %q, want it to contain the captured stderr", got.Error())
	}
	if !strings.Contains(got.Error(), base.Error()) {
		t.Errorf("withHelperStderr = %q, want it to still contain the base error", got.Error())
	}
}

func TestWithHelperStderrPassesThroughWhenAbsent(t *testing.T) {
	base := os.ErrNotExist
	got := withHelperStderr(base, filepath.Join(t.TempDir(), "nope.log"))
	if got != base {
		t.Errorf("withHelperStderr with no captured stderr = %v, want the base error unchanged", got)
	}
}

// TestStartVMSurfacesHelperStderrOnFailure exercises the real StartVM path
// against a fake "mvm-vz" that fails before ever printing a status line
// (mirroring Create.swift's `throw error` path, which fputs to stderr and
// exits nonzero without printing JSON). Regression coverage for two things
// at once: (1) the failure's stderr message must still reach the caller's
// error even though it's no longer piped straight to os.Stderr, and (2) the
// stderr must land in the per-VM log file rather than being lost.
func TestStartVMSurfacesHelperStderrOnFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-mvm-vz")
	const scriptBody = "#!/bin/sh\necho 'boom: kernel not found' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}

	b := &AppleVZBackend{binary: script, dataDir: dir, cacheDir: filepath.Join(dir, "cache")}
	_, err := b.StartVM("t1", "/kernel", "/rootfs", "console=ttyS0", "", 1, 128, nil, "")
	if err == nil {
		t.Fatal("StartVM: want error from a failing helper, got nil")
	}
	if !strings.Contains(err.Error(), "boom: kernel not found") {
		t.Fatalf("StartVM error = %q, want it to include the helper's stderr message", err.Error())
	}

	stderrLog := filepath.Join(dir, "vms", "t1", "mvm-vz-stderr.log")
	data, rerr := os.ReadFile(stderrLog)
	if rerr != nil {
		t.Fatalf("read captured stderr log: %v", rerr)
	}
	if !strings.Contains(string(data), "boom: kernel not found") {
		t.Fatalf("stderr log content = %q, want it to contain the helper's message", data)
	}
}

// === ResolveKernel (moved from internal/cli/start_test.go) ===

func TestResolveKernelPrefersCustomKernel(t *testing.T) {
	cacheDir := t.TempDir()
	custom := filepath.Join(cacheDir, "vmlinux-applevz")
	if err := os.WriteFile(custom, []byte("fake kernel"), 0o644); err != nil {
		t.Fatalf("write fake custom kernel: %v", err)
	}
	// Shared kernel present too, to confirm the custom one still wins.
	if err := os.WriteFile(filepath.Join(cacheDir, "vmlinux"), []byte("fake shared kernel"), 0o644); err != nil {
		t.Fatalf("write fake shared kernel: %v", err)
	}

	kernelPath, warning := ResolveKernel(cacheDir)
	if kernelPath != custom {
		t.Errorf("kernelPath = %q, want %q", kernelPath, custom)
	}
	if warning != "" {
		t.Errorf("warning = %q, want empty when custom kernel exists", warning)
	}
}

func TestResolveKernelFallsBackWhenCustomKernelMissing(t *testing.T) {
	cacheDir := t.TempDir()
	shared := filepath.Join(cacheDir, "vmlinux")
	if err := os.WriteFile(shared, []byte("fake shared kernel"), 0o644); err != nil {
		t.Fatalf("write fake shared kernel: %v", err)
	}

	kernelPath, warning := ResolveKernel(cacheDir)
	if kernelPath != shared {
		t.Errorf("kernelPath = %q, want %q (fallback to shared vmlinux)", kernelPath, shared)
	}
	if warning == "" {
		t.Error("warning = \"\", want a non-empty warning when falling back")
	}
	if !strings.Contains(warning, "vmlinux-applevz") {
		t.Errorf("warning = %q, want it to mention the missing custom kernel path", warning)
	}
}

// === ImageFileName / ResolveImage (moved from internal/cli/start_test.go) ===

func TestImageFileName(t *testing.T) {
	if got := ImageFileName(""); got != "base.ext4" {
		t.Errorf(`ImageFileName("") = %q, want base.ext4`, got)
	}
	if got := ImageFileName("my-image"); got != "my-image.ext4" {
		t.Errorf(`ImageFileName("my-image") = %q, want my-image.ext4`, got)
	}
}

func TestResolveImageDefaultsToBase(t *testing.T) {
	path, err := ResolveImage("/cache", "", nil)
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if path != "/cache/base.ext4" {
		t.Errorf("path = %q, want /cache/base.ext4", path)
	}
}

func TestResolveImageUsesLocalCacheWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "my-image.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetchCalled := false
	path, err := ResolveImage(dir, "my-image", func(image, dest string) error {
		fetchCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if fetchCalled {
		t.Error("fetch was called for an already-cached image")
	}
	if path != filepath.Join(dir, "my-image.ext4") {
		t.Errorf("path = %q, want the cached path", path)
	}
}

func TestResolveImageFetchesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	var fetchedImage, fetchedDest string
	path, err := ResolveImage(dir, "my-image", func(image, dest string) error {
		fetchedImage, fetchedDest = image, dest
		return os.WriteFile(dest, []byte("fetched"), 0o644)
	})
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if fetchedImage != "my-image" || fetchedDest != path {
		t.Errorf("fetch(%q, %q), want (my-image, %q)", fetchedImage, fetchedDest, path)
	}
}

func TestResolveImageErrorsWhenNoDaemonAvailable(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveImage(dir, "my-image", nil)
	if err == nil {
		t.Fatal("ResolveImage() = nil, want an error when the image is missing and there's no way to fetch it")
	}
	if !strings.Contains(err.Error(), "my-image") {
		t.Errorf("err = %v, want it to name the missing image", err)
	}
}
