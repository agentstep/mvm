//go:build linux

package uffd

import (
	"context"
	"unsafe"
)

// unsafeSizeof returns the byte size of v's type. Used in struct-layout
// tests to assert our Go structs match the kernel's C structs.
func unsafeSizeof(v any) uintptr {
	switch v := v.(type) {
	case uffdMsg:
		return unsafe.Sizeof(v)
	case uffdioAPI:
		return unsafe.Sizeof(v)
	case uffdioCopy:
		return unsafe.Sizeof(v)
	case uffdioRegister:
		return unsafe.Sizeof(v)
	case uffdioRange:
		return unsafe.Sizeof(v)
	case uffdPagefault:
		return unsafe.Sizeof(v)
	case uffdRemove:
		return unsafe.Sizeof(v)
	default:
		return 0
	}
}

// cancelledCtx returns a context that is already cancelled.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
