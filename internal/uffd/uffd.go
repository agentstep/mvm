//go:build linux

// Package uffd implements a userfaultfd page-fault handler for Firecracker
// snapshot restore. It provides thin syscall wrappers around the kernel
// UFFD ioctls and a handler event loop that services page faults from a
// memory-file backing store.
package uffd

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Constants mirror <linux/userfaultfd.h>. The UFFD ABI has been stable since
// Linux 4.11. ioctl numbers are computed via _IOWR('U', n, sizeof(struct)) and
// are independent of CPU architecture (the encoding is the same on amd64 and
// arm64).
const (
	// UFFD_API is the magic API version handshake value.
	UFFD_API uint64 = 0xAA

	// ioctl numbers. Values are for the current stable UFFD ABI. Verified
	// against the Linux 6.x kernel headers and the Rust userfaultfd crate.
	UFFDIO_API        uintptr = 0xc018aa3f
	UFFDIO_REGISTER   uintptr = 0xc020aa00
	UFFDIO_UNREGISTER uintptr = 0x8010aa01
	UFFDIO_WAKE       uintptr = 0x8010aa02
	UFFDIO_COPY       uintptr = 0xc028aa03
	UFFDIO_ZEROPAGE   uintptr = 0xc020aa04

	// Register mode flags.
	UFFDIO_REGISTER_MODE_MISSING uint64 = 1 << 0
	UFFDIO_REGISTER_MODE_WP      uint64 = 1 << 1

	// Event types from the uffd_msg.event byte.
	UFFD_EVENT_PAGEFAULT uint8 = 0x12
	UFFD_EVENT_FORK      uint8 = 0x13
	UFFD_EVENT_REMAP     uint8 = 0x14
	UFFD_EVENT_REMOVE    uint8 = 0x15
	UFFD_EVENT_UNMAP     uint8 = 0x16
)

// uffdioAPI is the argument to the UFFDIO_API ioctl. Layout matches
// `struct uffdio_api` in <linux/userfaultfd.h> (24 bytes).
type uffdioAPI struct {
	API      uint64
	Features uint64
	Ioctls   uint64
}

// uffdioRange describes an address range. Layout matches `struct uffdio_range`
// (16 bytes).
type uffdioRange struct {
	Start uint64
	Len   uint64
}

// uffdioRegister is the argument to UFFDIO_REGISTER. Layout matches
// `struct uffdio_register` (32 bytes).
type uffdioRegister struct {
	Range  uffdioRange
	Mode   uint64
	Ioctls uint64
}

// uffdioCopy is the argument to UFFDIO_COPY. Layout matches
// `struct uffdio_copy` (40 bytes). Copy holds the return value written by
// the kernel on success or a negative errno on failure.
type uffdioCopy struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// uffdMsg is the event record read from the UFFD fd. Layout matches
// `struct uffd_msg` (32 bytes, __packed). The Arg field is a byte buffer
// reinterpreted based on the Event discriminator.
type uffdMsg struct {
	Event     uint8
	Reserved1 uint8
	Reserved2 uint16
	Reserved3 uint32
	Arg       [24]byte
}

// uffdPagefault reinterprets uffdMsg.Arg when Event == UFFD_EVENT_PAGEFAULT.
// Layout matches the anonymous `pagefault` struct in the kernel union
// (packed: flags(8) + address(8) + ptid(4) = 20 bytes within a 24-byte union).
type uffdPagefault struct {
	Flags   uint64
	Address uint64
	Ptid    uint32
	_       uint32 // padding within the 24-byte union slot
}

// uffdRemove reinterprets uffdMsg.Arg when Event == UFFD_EVENT_REMOVE.
type uffdRemove struct {
	Start uint64
	End   uint64
}

// ioctl wraps syscall.Syscall(SYS_IOCTL, ...) and converts the raw errno to
// a Go error.
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

// APIHandshake performs the UFFDIO_API handshake on the given UFFD fd. It
// must be called exactly once on a fresh fd before any other UFFD operation.
// Firecracker does this before sending us the fd, so in practice the handler
// never calls this function — it is exposed for completeness and tests.
func APIHandshake(fd int) error {
	api := uffdioAPI{API: UFFD_API}
	return ioctl(fd, UFFDIO_API, unsafe.Pointer(&api))
}

// Copy issues UFFDIO_COPY to populate [dst, dst+length) with bytes read from
// [src, src+length). Returns the number of bytes actually copied by the kernel.
//
// On EAGAIN the caller should retry (the kernel has a pending REMOVE event
// that must be drained first). On EEXIST the page was resolved concurrently
// by another vCPU fault — that is benign and the caller should continue.
func Copy(fd int, dst, src, length uintptr) (int64, error) {
	c := uffdioCopy{
		Dst: uint64(dst),
		Src: uint64(src),
		Len: uint64(length),
	}
	if err := ioctl(fd, UFFDIO_COPY, unsafe.Pointer(&c)); err != nil {
		return c.Copy, err
	}
	return c.Copy, nil
}

// Wake issues UFFDIO_WAKE to wake any threads blocked on a page fault in
// [start, start+length). Used when UFFDIO_COPY failed and we want to unblock
// the guest so it re-raises the fault.
func Wake(fd int, start, length uintptr) error {
	r := uffdioRange{
		Start: uint64(start),
		Len:   uint64(length),
	}
	return ioctl(fd, UFFDIO_WAKE, unsafe.Pointer(&r))
}

// Unregister issues UFFDIO_UNREGISTER for [start, start+length). Used in
// response to UFFD_EVENT_REMOVE (only emitted when a balloon device is in
// use; we include it defensively).
func Unregister(fd int, start, length uintptr) error {
	r := uffdioRange{
		Start: uint64(start),
		Len:   uint64(length),
	}
	return ioctl(fd, UFFDIO_UNREGISTER, unsafe.Pointer(&r))
}

// ReadMsg reads a single uffdMsg from the UFFD fd. The read is blocking; a
// short read is treated as an error. EOF is returned as io.EOF via the
// underlying syscall.
func ReadMsg(fd int) (*uffdMsg, error) {
	var msg uffdMsg
	buf := (*[unsafe.Sizeof(msg)]byte)(unsafe.Pointer(&msg))[:]
	n, err := unix.Read(fd, buf)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// EOF — Firecracker closed the UFFD fd (VM exited).
		return nil, unix.EIO
	}
	if n != int(unsafe.Sizeof(msg)) {
		return nil, fmt.Errorf("short uffd_msg read: got %d bytes, want %d", n, unsafe.Sizeof(msg))
	}
	return &msg, nil
}

// AsPagefault reinterprets the Arg payload of a PAGEFAULT message.
func (m *uffdMsg) AsPagefault() *uffdPagefault {
	return (*uffdPagefault)(unsafe.Pointer(&m.Arg[0]))
}

// AsRemove reinterprets the Arg payload of a REMOVE message.
func (m *uffdMsg) AsRemove() *uffdRemove {
	return (*uffdRemove)(unsafe.Pointer(&m.Arg[0]))
}
