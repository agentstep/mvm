//go:build linux

package uffd

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGuestRegionUffdMappingJSON(t *testing.T) {
	// Verify the JSON shape matches what Firecracker emits (snake_case
	// field names, integer values).
	want := `[{"base_host_virt_addr":140234567680000,"size":2147483648,"offset":0,"page_size":4096}]`
	in := []GuestRegionUffdMapping{{
		BaseHostVirtAddr: 140234567680000,
		Size:             2147483648,
		Offset:           0,
		PageSize:         4096,
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != want {
		t.Errorf("marshal mismatch:\n got=%s\nwant=%s", b, want)
	}

	// Round-trip from the Rust-shaped JSON.
	var out []GuestRegionUffdMapping
	if err := json.Unmarshal([]byte(want), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 || out[0] != in[0] {
		t.Errorf("unmarshal mismatch: got=%+v want=%+v", out, in)
	}
}

func TestContainsAndFindRegion(t *testing.T) {
	regs := []GuestRegionUffdMapping{
		{BaseHostVirtAddr: 0x1000, Size: 0x1000, Offset: 0, PageSize: 4096},
		{BaseHostVirtAddr: 0x10000, Size: 0x2000, Offset: 0x1000, PageSize: 4096},
	}
	cases := []struct {
		addr      uint64
		wantIndex int // -1 => nil
	}{
		{0x1000, 0},
		{0x1fff, 0},
		{0x2000, -1}, // MMIO gap
		{0x10000, 1},
		{0x11fff, 1},
		{0x12000, -1},
		{0x0, -1},
	}
	for _, c := range cases {
		got := findRegion(regs, c.addr)
		if c.wantIndex < 0 {
			if got != nil {
				t.Errorf("addr=%#x: got %+v, want nil", c.addr, got)
			}
			continue
		}
		if got == nil || got != &regs[c.wantIndex] {
			t.Errorf("addr=%#x: got %+v, want regs[%d]", c.addr, got, c.wantIndex)
		}
	}
}

// TestRecvWithFdSocketpair verifies the SCM_RIGHTS receive path using a
// real socketpair. A fake "firecracker" sends a JSON body plus a file
// descriptor (pointing at /dev/null here) exactly the way the real thing
// does via sendmsg(2).
func TestRecvWithFdSocketpair(t *testing.T) {
	// socketpair(AF_UNIX, SOCK_STREAM).
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	// fds[0] is the "handler" side; fds[1] is the "firecracker" side.
	defer unix.Close(fds[1])

	// Wrap fds[0] in a *net.UnixConn so recvWithFd can consume it via
	// ReadMsgUnix. We do this through net.FileConn, which dup's the fd
	// and takes ownership of the dup — meaning we should close the
	// original.
	f := os.NewFile(uintptr(fds[0]), "sp0")
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}
	defer c.Close()
	uc := c.(*net.UnixConn)

	// Open /dev/null to send as the "uffd" fd.
	uffdFd, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer unix.Close(uffdFd)

	// Construct the SCM_RIGHTS cmsg and send the JSON body.
	body := []byte(`[{"base_host_virt_addr":4096,"size":4096,"offset":0,"page_size":4096}]`)
	rights := unix.UnixRights(uffdFd)
	if err := unix.Sendmsg(fds[1], body, rights, nil, 0); err != nil {
		t.Fatalf("sendmsg: %v", err)
	}

	// Receive on the handler side.
	gotBody, gotFd, err := recvWithFd(uc)
	if err != nil {
		t.Fatalf("recvWithFd: %v", err)
	}
	if gotFd < 0 {
		t.Fatalf("expected an fd, got -1")
	}
	defer unix.Close(gotFd)
	if string(gotBody) != string(body) {
		t.Errorf("body mismatch: got=%q want=%q", gotBody, body)
	}

	// The dup'd fd should refer to a valid open file — fstat must succeed.
	var st unix.Stat_t
	if err := unix.Fstat(gotFd, &st); err != nil {
		t.Errorf("fstat(received fd): %v", err)
	}
}

// TestRecvWithFdNoCmsg verifies that a message with no ancillary data
// returns fd=-1 with no error, so the caller can retry.
func TestRecvWithFdNoCmsg(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer unix.Close(fds[1])

	f := os.NewFile(uintptr(fds[0]), "sp0")
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}
	defer c.Close()
	uc := c.(*net.UnixConn)

	body := []byte(`[]`)
	if err := unix.Sendmsg(fds[1], body, nil, nil, 0); err != nil {
		t.Fatalf("sendmsg: %v", err)
	}

	gotBody, gotFd, err := recvWithFd(uc)
	if err != nil {
		t.Fatalf("recvWithFd: %v", err)
	}
	if gotFd != -1 {
		t.Errorf("expected fd=-1 (no cmsg), got %d", gotFd)
		_ = unix.Close(gotFd)
	}
	if string(gotBody) != string(body) {
		t.Errorf("body mismatch: got=%q want=%q", gotBody, body)
	}
}

// TestMmapFile checks the backing-file mmap helper against a small test file.
func TestMmapFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.bin")
	content := []byte("pagecontents\x00\x01\x02")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	data, size, err := mmapFile(path)
	if err != nil {
		t.Fatalf("mmapFile: %v", err)
	}
	defer unix.Munmap(data[:size:size])
	if size != len(content) {
		t.Errorf("size: got %d want %d", size, len(content))
	}
	if string(data[:size]) != string(content) {
		t.Errorf("contents mismatch")
	}
}

// TestUffdMsgSize asserts the on-wire uffd_msg struct size matches the kernel
// definition (32 bytes: 8-byte header + 24-byte arg union).
func TestUffdMsgSize(t *testing.T) {
	var m uffdMsg
	const want = 32
	if got := int(unsafeSizeof(m)); got != want {
		t.Errorf("sizeof(uffdMsg) = %d, want %d", got, want)
	}
}

// TestUffdioAPISize asserts the handshake struct size matches the kernel
// definition used to compute UFFDIO_API = 0xc018aa3f (0x18 = 24).
func TestUffdioAPISize(t *testing.T) {
	var a uffdioAPI
	const want = 24
	if got := int(unsafeSizeof(a)); got != want {
		t.Errorf("sizeof(uffdioAPI) = %d, want %d", got, want)
	}
}

// TestUffdioCopySize asserts sizeof matches UFFDIO_COPY = 0xc028aa03 (0x28 = 40).
func TestUffdioCopySize(t *testing.T) {
	var c uffdioCopy
	const want = 40
	if got := int(unsafeSizeof(c)); got != want {
		t.Errorf("sizeof(uffdioCopy) = %d, want %d", got, want)
	}
}

// TestUffdioRegisterSize asserts sizeof matches UFFDIO_REGISTER = 0xc020aa00 (0x20 = 32).
func TestUffdioRegisterSize(t *testing.T) {
	var r uffdioRegister
	const want = 32
	if got := int(unsafeSizeof(r)); got != want {
		t.Errorf("sizeof(uffdioRegister) = %d, want %d", got, want)
	}
}

// TestHandlerRunContextCancel verifies that Run exits when the context is
// cancelled before Firecracker connects.
func TestHandlerRunContextCancel(t *testing.T) {
	dir := t.TempDir()
	// mem.bin is required even though Run never touches it before accept.
	memPath := filepath.Join(dir, "mem.bin")
	if err := os.WriteFile(memPath, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("write mem: %v", err)
	}
	h := &Handler{
		SocketPath:  filepath.Join(dir, "uffd.sock"),
		MemFilePath: memPath,
	}

	done := make(chan error, 1)
	go func() {
		done <- h.Run(cancelledCtx())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected non-nil error on cancelled ctx")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return after cancel within 5s")
	}
}
