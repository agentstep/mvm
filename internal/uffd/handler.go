//go:build linux

package uffd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// GuestRegionUffdMapping describes one contiguous guest-memory region as
// registered with the UFFD by Firecracker. The JSON tags MUST match what the
// Rust side (firecracker/src/firecracker/examples/uffd/uffd_utils.rs) emits
// so that encoding/json decodes cleanly.
type GuestRegionUffdMapping struct {
	// BaseHostVirtAddr is the virtual address at which Firecracker mmap'd
	// this region in its own address space. Page-fault addresses delivered
	// by the kernel fall within [BaseHostVirtAddr, BaseHostVirtAddr+Size).
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	// Size is the region length in bytes.
	Size uint64 `json:"size"`
	// Offset is the byte offset into the memory dump file where this
	// region's data begins.
	Offset uint64 `json:"offset"`
	// PageSize is the fault granularity — 4096 for normal guests; larger
	// for hugepage-backed guests (we don't use hugepages today).
	PageSize uint64 `json:"page_size"`
}

// contains returns true if addr falls within this region.
func (r *GuestRegionUffdMapping) contains(addr uint64) bool {
	return addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size
}

// Handler services page-faults for a single VM. It is single-use: call Run
// once, and the handler exits when either the VM exits (Firecracker closes
// the UFFD fd) or the context is cancelled.
type Handler struct {
	// SocketPath is the filesystem path of the Unix domain socket we bind
	// and Firecracker connects to. Must be under 108 bytes total.
	SocketPath string
	// MemFilePath is the path to the decrypted memory dump file (mem.bin)
	// that backs the guest's RAM.
	MemFilePath string

	// Logger, optional. Defaults to the standard library logger writing to
	// stderr if nil.
	Logger *log.Logger
}

const (
	// recvRetries is how many times we retry recvmsg when Firecracker's
	// initial send arrives without the cmsg (observed intermittently, also
	// seen in the Rust reference handler).
	recvRetries = 5
	// recvRetryDelay is the sleep between retries.
	recvRetryDelay = 100 * time.Millisecond
)

// Run accepts a Firecracker connection, receives the guest memory mappings
// and UFFD fd, then enters the page-fault loop. It returns nil when
// Firecracker exits cleanly (EOF on UFFD fd) and a non-nil error on any
// protocol or syscall failure.
//
// On panic, Run makes a best effort to SIGKILL Firecracker so the guest
// does not hang forever on the next page fault.
func (h *Handler) Run(ctx context.Context) (retErr error) {
	logger := h.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "[mvm-uffd] ", log.LstdFlags|log.Lmicroseconds)
	}

	// 1. Set up the listening socket.
	// Remove stale socket if present (previous run may have crashed).
	_ = os.Remove(h.SocketPath)
	l, err := net.Listen("unix", h.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", h.SocketPath, err)
	}
	defer l.Close()
	// Make the socket world-readable so Firecracker (possibly running as
	// a different uid than the handler) can connect.
	if err := os.Chmod(h.SocketPath, 0o666); err != nil {
		return fmt.Errorf("chmod %s: %w", h.SocketPath, err)
	}

	// 2. mmap the memory dump read-only, lazy (no MAP_POPULATE). Pages
	// fault in from disk on demand via the page cache.
	backing, backingLen, err := mmapFile(h.MemFilePath)
	if err != nil {
		return fmt.Errorf("mmap %s: %w", h.MemFilePath, err)
	}
	defer unix.Munmap(backing[:backingLen:backingLen])

	// 3. Accept Firecracker's connection. Plumb ctx cancellation by
	// closing the listener — Accept then returns an error.
	acceptCh := make(chan acceptResult, 1)
	go func() {
		c, err := l.Accept()
		acceptCh <- acceptResult{c, err}
	}()

	var conn net.Conn
	select {
	case <-ctx.Done():
		l.Close()
		return ctx.Err()
	case r := <-acceptCh:
		if r.err != nil {
			return fmt.Errorf("accept: %w", r.err)
		}
		conn = r.conn
	}
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("unexpected connection type %T", conn)
	}

	// 4. Receive the JSON mappings + UFFD fd via SCM_RIGHTS. Retry a few
	// times because the Rust reference handler does (and documents flaky
	// receives on initial connect).
	var uffdFd int = -1
	var body []byte
	for attempt := 0; attempt < recvRetries; attempt++ {
		b, fd, err := recvWithFd(uc)
		if err == nil && fd >= 0 {
			uffdFd = fd
			body = b
			break
		}
		if err != nil {
			logger.Printf("recvmsg attempt %d: %v", attempt+1, err)
		}
		time.Sleep(recvRetryDelay)
	}
	if uffdFd < 0 {
		return fmt.Errorf("uffd fd not received after %d retries", recvRetries)
	}
	defer unix.Close(uffdFd)

	// 5. Parse the JSON body into mappings.
	var mappings []GuestRegionUffdMapping
	if err := json.Unmarshal(body, &mappings); err != nil {
		return fmt.Errorf("parse mappings: %w (body=%q)", err, string(body))
	}
	if len(mappings) == 0 {
		return errors.New("received empty mappings list")
	}
	logger.Printf("received %d mapping(s), uffd fd=%d", len(mappings), uffdFd)
	for i, m := range mappings {
		logger.Printf("  region[%d] base=%#x size=%d offset=%d page=%d",
			i, m.BaseHostVirtAddr, m.Size, m.Offset, m.PageSize)
	}

	// Sanity-check offsets against the backing file size.
	for i, m := range mappings {
		if m.Offset+m.Size > uint64(backingLen) {
			return fmt.Errorf("region[%d] offset=%d size=%d exceeds mem file size %d",
				i, m.Offset, m.Size, backingLen)
		}
	}

	// 6. Record Firecracker's pid (via SO_PEERCRED) so the panic hook
	// can kill it if the handler crashes.
	fcPid := getPeerPid(uc)
	logger.Printf("firecracker pid (via SO_PEERCRED) = %d", fcPid)

	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC in page-fault loop: %v", r)
			if fcPid > 0 {
				logger.Printf("killing firecracker pid %d to avoid guest hang", fcPid)
				_ = syscall.Kill(fcPid, syscall.SIGKILL)
			}
			retErr = fmt.Errorf("handler panic: %v", r)
		}
	}()

	// 7. Page-fault loop. Context cancellation is handled by closing the
	// UFFD fd from a watcher goroutine.
	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		select {
		case <-ctx.Done():
			// Closing the fd makes any in-flight ReadMsg return an
			// error; the main loop then exits.
			_ = unix.Close(uffdFd)
		case <-stopCh:
		}
	}()

	backingBase := uintptr(unsafe.Pointer(&backing[0]))
	var faultsServed uint64
	for {
		msg, err := ReadMsg(uffdFd)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, unix.EIO) || errors.Is(err, unix.EBADF) {
				logger.Printf("uffd fd closed (EOF / VM exited); served %d faults", faultsServed)
				return nil
			}
			// Context cancel path.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read uffd_msg: %w", err)
		}

		switch msg.Event {
		case UFFD_EVENT_PAGEFAULT:
			pf := msg.AsPagefault()
			if err := h.servePageFault(uffdFd, pf.Address, mappings, backingBase); err != nil {
				return fmt.Errorf("serve pagefault at %#x: %w", pf.Address, err)
			}
			faultsServed++
		case UFFD_EVENT_REMOVE:
			// Only balloon devices trigger REMOVE; we don't use
			// balloon. Unregister defensively so the kernel stops
			// routing faults for that range to us.
			rm := msg.AsRemove()
			length := rm.End - rm.Start
			if err := Unregister(uffdFd, uintptr(rm.Start), uintptr(length)); err != nil {
				logger.Printf("UFFDIO_UNREGISTER(%#x, %d) failed: %v", rm.Start, length, err)
			}
		default:
			logger.Printf("ignoring uffd event 0x%x", msg.Event)
		}
	}
}

// servePageFault handles a single UFFD_EVENT_PAGEFAULT by locating the
// containing region, computing the offset into the backing mmap, and issuing
// UFFDIO_COPY. EEXIST and EAGAIN are handled per the kernel protocol.
func (h *Handler) servePageFault(uffdFd int, faultAddr uint64, mappings []GuestRegionUffdMapping, backingBase uintptr) error {
	region := findRegion(mappings, faultAddr)
	if region == nil {
		return fmt.Errorf("no region contains fault addr %#x", faultAddr)
	}
	pageSize := region.PageSize
	// Align to page boundary — the kernel fault address is sub-page.
	alignedAddr := faultAddr &^ (pageSize - 1)
	offInRegion := alignedAddr - region.BaseHostVirtAddr
	src := backingBase + uintptr(region.Offset) + uintptr(offInRegion)

	_, err := Copy(uffdFd, uintptr(alignedAddr), src, uintptr(pageSize))
	if err == nil {
		return nil
	}
	// EEXIST: another vCPU fault concurrently populated this page. Benign.
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	// EAGAIN: the kernel has a pending REMOVE event; the caller should
	// loop again (we'll drain the REMOVE on the next ReadMsg and the
	// guest will re-raise the fault).
	if errors.Is(err, unix.EAGAIN) {
		return nil
	}
	return err
}

// findRegion returns the first mapping that contains addr, or nil.
func findRegion(mappings []GuestRegionUffdMapping, addr uint64) *GuestRegionUffdMapping {
	for i := range mappings {
		if mappings[i].contains(addr) {
			return &mappings[i]
		}
	}
	return nil
}

// mmapFile opens path read-only and mmaps the entire file with
// PROT_READ|MAP_PRIVATE (no MAP_POPULATE — we want lazy page-cache
// population so cold restore doesn't pre-fault 2GB of disk).
func mmapFile(path string) ([]byte, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	// The mmap holds the file open for us; we can close the fd.
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := int(fi.Size())
	if size == 0 {
		return nil, 0, fmt.Errorf("%s is empty", path)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return nil, 0, err
	}
	return data, size, nil
}

// acceptResult bundles the result of a net.Listener.Accept call.
type acceptResult struct {
	conn net.Conn
	err  error
}

// recvWithFd performs a single recvmsg on uc and returns (body, fd, err).
// If the message arrived without an SCM_RIGHTS cmsg, fd is -1 and err is nil
// (the caller retries).
func recvWithFd(uc *net.UnixConn) ([]byte, int, error) {
	oob := make([]byte, unix.CmsgSpace(4)) // one int (fd)
	buf := make([]byte, 64*1024)           // mappings JSON is ~100 bytes; 64K is ample
	n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, -1, err
	}
	if oobn == 0 {
		return buf[:n], -1, nil
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return buf[:n], -1, fmt.Errorf("parse cmsg: %w", err)
	}
	for _, scm := range scms {
		if scm.Header.Level != unix.SOL_SOCKET || scm.Header.Type != unix.SCM_RIGHTS {
			continue
		}
		fds, err := unix.ParseUnixRights(&scm)
		if err != nil {
			return buf[:n], -1, fmt.Errorf("parse rights: %w", err)
		}
		if len(fds) >= 1 {
			// Close any extra fds we didn't expect.
			for _, extra := range fds[1:] {
				_ = unix.Close(extra)
			}
			return buf[:n], fds[0], nil
		}
	}
	return buf[:n], -1, nil
}

// getPeerPid returns the pid of the process on the other end of a connected
// Unix-domain stream socket, or 0 if we cannot determine it.
func getPeerPid(uc *net.UnixConn) int {
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int
	ctrlErr := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			return
		}
		pid = int(cred.Pid)
	})
	if ctrlErr != nil {
		return 0
	}
	return pid
}
