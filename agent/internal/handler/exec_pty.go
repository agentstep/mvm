//go:build linux

package handler

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// openPty allocates a PTY pair using /dev/ptmx (no CGO needed).
// Returns (masterFD, slaveFD, error).
func openPty(rows, cols uint16, term string) (master *os.File, slave *os.File, err error) {
	// Open the PTY master
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	// Unlock the slave (equivalent to unlockpt)
	// TIOCSPTLCK takes a pointer to an int; 0 = unlock
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(masterFD)
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}

	// Get the slave PTY number (equivalent to ptsname)
	ptsNum, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		unix.Close(masterFD)
		return nil, nil, fmt.Errorf("ptsname: %w", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", ptsNum)

	// Open the slave
	slaveFD, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		unix.Close(masterFD)
		return nil, nil, fmt.Errorf("open slave %s: %w", slavePath, err)
	}

	// Set initial terminal size
	ws := &unix.Winsize{
		Row: rows,
		Col: cols,
	}
	if err := unix.IoctlSetWinsize(slaveFD, unix.TIOCSWINSZ, ws); err != nil {
		unix.Close(masterFD)
		unix.Close(slaveFD)
		return nil, nil, fmt.Errorf("set winsize: %w", err)
	}

	master = os.NewFile(uintptr(masterFD), "/dev/ptmx")
	slave = os.NewFile(uintptr(slaveFD), slavePath)
	return master, slave, nil
}

// HandleExecPty runs a command in a PTY and relays I/O over the connection.
// It takes over the connection for the duration of the interactive session.
func HandleExecPty(conn io.ReadWriter, req *protocol.ExecPtyRequest, id string) {
	master, slave, err := openPty(req.Rows, req.Cols, req.Term)
	if err != nil {
		writeError(conn, id, err)
		return
	}
	defer master.Close()

	cmd := exec.Command("sh", "-c", req.Command)

	// Set up environment
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	term := req.Term
	if term == "" {
		term = "xterm-256color"
	}
	cmd.Env = append(cmd.Env, fmt.Sprintf("TERM=%s", term))

	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}

	// Attach the slave PTY as stdin/stdout/stderr
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    int(slave.Fd()),
	}

	if err := cmd.Start(); err != nil {
		slave.Close()
		writeError(conn, id, err)
		return
	}

	// Close slave in the parent — the child has its own copy
	slave.Close()

	// Send initial OK response to confirm PTY is ready
	var mu sync.Mutex
	mu.Lock()
	protocol.WriteFrame(conn, &protocol.Response{
		Type: protocol.RespOK,
		ID:   id,
	})
	mu.Unlock()

	// Goroutine 1: Read from PTY master, write as stdout frames to conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				mu.Lock()
				writeErr := protocol.WriteFrame(conn, &protocol.Response{
					Type: protocol.RespStdout,
					ID:   id,
					Data: data,
				})
				mu.Unlock()
				if writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Goroutine 2: Read request frames from conn, handle stdin and resize
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			var frame protocol.Response
			if err := protocol.ReadFrame(conn, &frame); err != nil {
				// Connection closed or error — signal the process to exit
				// Send SIGHUP which is what a terminal closing does
				if cmd.Process != nil {
					cmd.Process.Signal(syscall.SIGHUP)
				}
				return
			}

			switch frame.Type {
			case protocol.RespStdin:
				if len(frame.Data) > 0 {
					master.Write(frame.Data)
				}
			case protocol.RespResize:
				// ExitCode encodes rows and cols as (rows << 16 | cols)
				if frame.ExitCode > 0 {
					rows := uint16(frame.ExitCode >> 16)
					cols := uint16(frame.ExitCode & 0xFFFF)
					ws := &unix.Winsize{Row: rows, Col: cols}
					unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws)
				}
			}
		}
	}()

	// Wait for the command to finish
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	// Wait for the PTY reader to drain
	// Close master to signal EOF to the reader goroutine
	master.Close()
	wg.Wait()

	// Send final exit frame
	mu.Lock()
	err = protocol.WriteFrame(conn, &protocol.Response{
		Type:     protocol.RespExit,
		ID:       id,
		ExitCode: exitCode,
	})
	mu.Unlock()
	if err != nil {
		log.Printf("failed to write exit frame: %v", err)
	}
}
