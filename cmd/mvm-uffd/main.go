//go:build linux

// mvm-uffd is the userfaultfd page-fault handler sidecar that serves memory
// pages into a Firecracker guest during snapshot restore. It is launched by
// the mvm daemon just before issuing the PUT /snapshot/load API call with
// backend_type=Uffd.
//
// Usage:
//
//	mvm-uffd --socket=/run/mvm/<vm>-uffd.sock --mem=/var/lib/mvm/<vm>/mem.bin
//
// The process exits cleanly (status 0) when Firecracker exits and closes the
// UFFD fd. It exits non-zero on any protocol or syscall failure. On
// unexpected panics it sends SIGKILL to Firecracker (located via SO_PEERCRED
// on the accepted socket) so the guest does not hang forever on the next
// page fault.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentstep/mvm/internal/uffd"
)

func main() {
	log.SetPrefix("[mvm-uffd] ")
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	var (
		socketPath = flag.String("socket", "", "path to the unix socket to bind for Firecracker")
		memPath    = flag.String("mem", "", "path to the memory dump file (mem.bin)")
	)
	flag.Parse()

	if *socketPath == "" || *memPath == "" {
		fmt.Fprintln(os.Stderr, "usage: mvm-uffd --socket=PATH --mem=PATH")
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h := &uffd.Handler{
		SocketPath:  *socketPath,
		MemFilePath: *memPath,
		Logger:      log.Default(),
	}

	log.Printf("starting: socket=%s mem=%s", *socketPath, *memPath)
	if err := h.Run(ctx); err != nil {
		log.Printf("handler exited with error: %v", err)
		os.Exit(1)
	}
	log.Printf("handler exited cleanly")
}
