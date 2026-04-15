package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
)

const snapshotsBaseDir = "/opt/mvm/snapshots"

// --- Request/Response types ---

type CreateVMRequest struct {
	Name      string          `json:"name"`
	Cpus      int             `json:"cpus,omitempty"`
	MemoryMB  int             `json:"memory_mb,omitempty"`
	Ports     []state.PortMap `json:"ports,omitempty"`
	NetPolicy string          `json:"net_policy,omitempty"`
	Volumes   []string        `json:"volumes,omitempty"`
	Seccomp   string          `json:"seccomp,omitempty"`
}

type ExecRequest struct {
	Command     string `json:"command"`
	Stdin       string `json:"stdin,omitempty"`
	Stream      bool   `json:"stream,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
}

type ExecResponse struct {
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

type VMResponse struct {
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	GuestIP   string          `json:"guest_ip,omitempty"`
	PID       int             `json:"pid,omitempty"`
	Backend   string          `json:"backend,omitempty"`
	Ports     []state.PortMap `json:"ports,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Error     string          `json:"error,omitempty"`
}

// SnapshotCreateRequest is the optional body for POST /vms/{name}/snapshot.
type SnapshotCreateRequest struct {
	Name string `json:"name,omitempty"`
}

// SnapshotRestoreRequest is the body for POST /vms/{name}/restore.
type SnapshotRestoreRequest struct {
	Name string `json:"name"`
}

// SnapshotInfo describes a snapshot for listing.
type SnapshotInfo struct {
	Name    string `json:"name"`
	VM      string `json:"vm,omitempty"`
	Created string `json:"created,omitempty"`
	Type    string `json:"type,omitempty"`
}

// --- Handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.store.ListVMs()
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	result := make([]VMResponse, 0, len(vms))
	for _, vm := range vms {
		result = append(result, VMResponse{
			Name:      vm.Name,
			Status:    vm.Status,
			GuestIP:   vm.GuestIP,
			PID:       vm.PID,
			Backend:   vm.Backend,
			Ports:     vm.Ports,
			CreatedAt: vm.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req CreateVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, fmt.Errorf("invalid request: %w", err), http.StatusBadRequest)
		return
	}

	if err := state.ValidateName(req.Name); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}

	now := time.Now()
	vm := &state.VM{
		Name:      req.Name,
		Status:    "starting",
		Ports:     req.Ports,
		NetPolicy: req.NetPolicy,
		Cpus:      req.Cpus,
		MemoryMB:  req.MemoryMB,
		CreatedAt: now,
	}
	netIndex, err := s.store.ReserveVM(vm)
	if err != nil {
		httpError(w, err, http.StatusConflict)
		return
	}
	alloc := state.AllocateNet(netIndex)

	var pid int
	var socketPath string

	usePool := (req.Cpus <= 0 || req.Cpus == firecracker.GuestVcpuCount) &&
		(req.MemoryMB <= 0 || req.MemoryMB == firecracker.GuestMemSizeMiB)
	if usePool {
		claimedPid, claimedSocket, claimErr := firecracker.ClaimPoolSlot(s.executor, req.Name, alloc)
		if claimErr == nil && claimedPid > 0 {
			pid = claimedPid
			socketPath = claimedSocket
			firecracker.ReplenishPool(s.executor)
		}
	}

	if pid == 0 {
		socketPath = firecracker.SocketPath(req.Name)
		pid, err = firecracker.Start(s.executor, req.Name, alloc, req.Cpus, req.MemoryMB)
		if err != nil {
			s.store.RemoveVM(req.Name)
			httpError(w, err, http.StatusInternalServerError)
			return
		}
	}

	s.store.UpdateVM(req.Name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = alloc.TAPDev
		v.GuestMAC = alloc.GuestMAC
		v.SocketPath = socketPath
		v.PID = pid
		v.RootfsPath = firecracker.VMDir(req.Name) + "/rootfs.ext4"
	})

	go func() {
		if !firecracker.WaitForGuest(s.executor, alloc.GuestIP, 120*time.Second) {
			log.Printf("VM %s: guest agent not reachable after 120s", req.Name)
			return
		}
		firecracker.SetupGuestNetworkViaAgent(s.executor, alloc.GuestIP, alloc.TAPIP)

		// Reload VM state for post-boot setup (state was updated above).
		postVM, err := s.store.GetVM(req.Name)
		if err != nil {
			log.Printf("VM %s: failed to reload state for post-boot setup: %v", req.Name, err)
			return
		}

		if err := firecracker.SetupPortForwarding(s.executor, postVM); err != nil {
			log.Printf("VM %s: port forwarding setup failed: %v", req.Name, err)
		}
		if err := firecracker.ApplyNetworkPolicyViaAgent(s.executor, postVM); err != nil {
			log.Printf("VM %s: network policy setup failed: %v", req.Name, err)
		}
		if len(req.Volumes) > 0 {
			if err := firecracker.SetupVolumeMounts(s.executor, postVM, req.Volumes); err != nil {
				log.Printf("VM %s: volume mount setup failed: %v", req.Name, err)
			}
		}
		if req.Seccomp != "" {
			if err := firecracker.ApplySeccompViaAgent(s.executor, postVM, req.Seccomp); err != nil {
				log.Printf("VM %s: seccomp setup failed: %v", req.Name, err)
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMResponse{
		Name:      req.Name,
		Status:    "running",
		GuestIP:   alloc.GuestIP,
		PID:       pid,
		Ports:     req.Ports,
		CreatedAt: now,
	})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, fmt.Errorf("invalid request: %w", err), http.StatusBadRequest)
		return
	}

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	// Auto-resume paused VMs so idle-pause doesn't break exec.
	if vm.Status == "paused" {
		if err := firecracker.Resume(s.executor, vm); err != nil {
			httpError(w, fmt.Errorf("auto-resume failed: %w", err), http.StatusInternalServerError)
			return
		}
		s.store.UpdateVM(name, func(v *state.VM) { v.Status = "running" })
	} else if vm.Status != "running" {
		httpError(w, fmt.Errorf("VM %q is %s", name, vm.Status), http.StatusConflict)
		return
	}

	now := time.Now()
	s.store.UpdateVM(name, func(v *state.VM) { v.LastActivity = &now })

	if req.Interactive {
		s.handleInteractiveExec(w, r, name, req.Command)
		return
	}

	// Talk to the guest agent over Firecracker's vsock UDS bridge.
	client := agentclient.New(&agentclient.FirecrackerVsockDialer{
		UDSPath: firecracker.VsockUDSPath(name),
	})

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	result, execErr := client.Exec(ctx, req.Command, req.Stdin)
	if execErr != nil {
		httpError(w, execErr, http.StatusInternalServerError)
		return
	}

	if req.Stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)

		// Send output as a single stdout frame + exit frame.
		if result.Output != "" {
			frame, _ := json.Marshal(map[string]interface{}{"type": "stdout", "data": result.Output})
			w.Write(frame)
			w.Write([]byte("\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		exitFrame, _ := json.Marshal(map[string]interface{}{"type": "exit", "exit_code": result.ExitCode})
		w.Write(exitFrame)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ExecResponse{Output: result.Output, ExitCode: result.ExitCode})
}

// handleInteractiveExec hijacks the HTTP connection and bridges it to the
// guest agent's exec_pty endpoint. After the initial handshake, the daemon
// is a transparent bidirectional relay — length-prefixed JSON frames pass
// through unmodified between the CLI client and the guest agent.
func (s *Server) handleInteractiveExec(w http.ResponseWriter, r *http.Request, vmName, command string) {
	// 1. Dial the guest agent via vsock.
	dialer := &agentclient.FirecrackerVsockDialer{
		UDSPath: firecracker.VsockUDSPath(vmName),
	}
	agentConn, err := dialer.Dial(r.Context())
	if err != nil {
		httpError(w, fmt.Errorf("dial agent: %w", err), http.StatusInternalServerError)
		return
	}
	defer agentConn.Close()

	// 2. Send exec_pty request to the agent — must match the agent's
	// protocol.Request wire format: type + id at top level, pty nested.
	agentReq := struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Pty  struct {
			Command string `json:"command"`
			Rows    int    `json:"rows"`
			Cols    int    `json:"cols"`
			Term    string `json:"term,omitempty"`
		} `json:"pty"`
	}{
		Type: "exec_pty",
		ID:   agentclient.NewID(),
	}
	agentReq.Pty.Command = command
	agentReq.Pty.Rows = 24
	agentReq.Pty.Cols = 80
	agentReq.Pty.Term = "xterm-256color"
	if err := agentclient.WriteFrame(agentConn, agentReq); err != nil {
		httpError(w, fmt.Errorf("send exec_pty: %w", err), http.StatusInternalServerError)
		return
	}

	// 3. Read the agent's initial OK response.
	var agentResp agentclient.ExecPtyResponse
	if err := agentclient.ReadFrame(agentConn, &agentResp); err != nil {
		httpError(w, fmt.Errorf("read agent response: %w", err), http.StatusInternalServerError)
		return
	}
	if agentResp.Type != "ok" {
		httpError(w, fmt.Errorf("agent error: %s", agentResp.Error), http.StatusInternalServerError)
		return
	}

	// 4. Hijack the HTTP connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		httpError(w, fmt.Errorf("server does not support hijacking"), http.StatusInternalServerError)
		agentConn.Close()
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		httpError(w, fmt.Errorf("hijack: %w", err), http.StatusInternalServerError)
		agentConn.Close()
		return
	}
	defer conn.Close()

	// 5. Write HTTP 101 Switching Protocols response.
	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Upgrade: tty\r\n")
	bufrw.WriteString("\r\n")
	bufrw.Flush()

	// 6. Bidirectional relay: transparent bridge between client and agent.
	done := make(chan struct{}, 2)

	// Agent -> Client
	go func() {
		io.Copy(conn, agentConn)
		done <- struct{}{}
	}()

	// Client -> Agent
	go func() {
		io.Copy(agentConn, conn)
		done <- struct{}{}
	}()

	// Wait for either direction to finish, then clean up both.
	<-done
	conn.Close()
	agentConn.Close()
	<-done

	log.Printf("VM %s: interactive exec session ended", vmName)
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	firecracker.Cleanup(s.executor, vm)
	s.store.RemoveVM(name)

	w.WriteHeader(http.StatusNoContent)
}

type StopVMRequest struct {
	Force bool `json:"force,omitempty"`
}

func (s *Server) handleStopVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req StopVMRequest
	// Body is optional — default to graceful stop.
	json.NewDecoder(r.Body).Decode(&req)

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "running" && vm.Status != "paused" {
		httpError(w, fmt.Errorf("VM %q is %s", name, vm.Status), http.StatusConflict)
		return
	}

	// Remove port forwarding before stopping.
	firecracker.RemovePortForwarding(s.executor, vm)

	if req.Force {
		// Force kill — no graceful shutdown attempt.
		s.executor.Run(fmt.Sprintf("sudo kill -9 %d 2>/dev/null || true", vm.PID))
		s.executor.Run(fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true",
			firecracker.SocketPath(name), vm.TAPDevice))
	} else {
		// Resume paused VMs before graceful shutdown (needed for agent
		// to process the poweroff command).
		if vm.Status == "paused" {
			firecracker.Resume(s.executor, vm)
		}
		hostKeyPath := firecracker.KeyDir + "/mvm.id_ed25519"
		firecracker.StopViaAgent(s.executor, vm, hostKeyPath)
	}

	now := time.Now()
	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "stopped"
		v.StoppedAt = &now
	})

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePauseVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "running" {
		httpError(w, fmt.Errorf("VM %q is %s, cannot pause", name, vm.Status), http.StatusConflict)
		return
	}

	if err := firecracker.Pause(s.executor, vm); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	s.store.UpdateVM(name, func(v *state.VM) { v.Status = "paused" })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResumeVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "paused" {
		httpError(w, fmt.Errorf("VM %q is %s, cannot resume", name, vm.Status), http.StatusConflict)
		return
	}

	if err := firecracker.Resume(s.executor, vm); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	s.store.UpdateVM(name, func(v *state.VM) { v.Status = "running" })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePoolWarm(w http.ResponseWriter, r *http.Request) {
	go func() {
		if err := firecracker.WarmPool(s.executor); err != nil {
			log.Printf("pool warm failed: %v", err)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "warming"})
}

func (s *Server) handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	ready, total := firecracker.PoolStatus(s.executor)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"ready": ready, "total": total})
}

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "running" && vm.Status != "paused" {
		httpError(w, fmt.Errorf("VM %q is %s, must be running or paused", name, vm.Status), http.StatusConflict)
		return
	}

	var req SnapshotCreateRequest
	// Body is optional — ignore decode errors.
	json.NewDecoder(r.Body).Decode(&req)
	snapName := req.Name
	if snapName == "" {
		snapName = name + "-snap"
	}

	snapDir := filepath.Join(snapshotsBaseDir, snapName)
	if err := firecracker.SnapshotVM(s.executor, vm, snapDir); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"snapshot": snapName,
		"status":   "created",
	})
}

func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req SnapshotRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, fmt.Errorf("invalid request: %w", err), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		httpError(w, fmt.Errorf("snapshot name is required"), http.StatusBadRequest)
		return
	}

	snapDir := filepath.Join(snapshotsBaseDir, req.Name)
	if _, err := os.Stat(filepath.Join(snapDir, "meta.json")); err != nil {
		httpError(w, fmt.Errorf("snapshot %q not found", req.Name), http.StatusNotFound)
		return
	}

	// If VM exists and is running or paused, stop it first.
	vm, err := s.store.GetVM(name)
	if err == nil && (vm.Status == "running" || vm.Status == "paused") {
		firecracker.RemovePortForwarding(s.executor, vm)
		if vm.Status == "paused" {
			firecracker.Resume(s.executor, vm)
		}
		hostKeyPath := firecracker.KeyDir + "/mvm.id_ed25519"
		firecracker.StopViaAgent(s.executor, vm, hostKeyPath)
		now := time.Now()
		s.store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
	}

	// Remove existing VM entry if present so we can reserve a fresh one.
	if err == nil {
		s.store.RemoveVM(name)
	}

	// Reserve a new VM entry with a network allocation.
	newVM := &state.VM{
		Name:      name,
		Status:    "restoring",
		CreatedAt: time.Now(),
	}
	netIndex, err := s.store.ReserveVM(newVM)
	if err != nil {
		httpError(w, err, http.StatusConflict)
		return
	}
	alloc := state.AllocateNet(netIndex)

	pid, socketPath, err := firecracker.RestoreVMSnapshot(s.executor, name, snapDir, alloc)
	if err != nil {
		s.store.RemoveVM(name)
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = alloc.TAPDev
		v.GuestMAC = alloc.GuestMAC
		v.SocketPath = socketPath
		v.PID = pid
		v.RootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"snapshot": req.Name,
		"status":   "restored",
	})
}

func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	names, err := firecracker.ListSnapshots(snapshotsBaseDir)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	result := make([]SnapshotInfo, 0, len(names))
	for _, n := range names {
		info := SnapshotInfo{Name: n}
		metaPath := filepath.Join(snapshotsBaseDir, n, "meta.json")
		data, err := os.ReadFile(metaPath)
		if err == nil {
			var meta map[string]string
			if json.Unmarshal(data, &meta) == nil {
				info.VM = meta["vm"]
				info.Created = meta["created"]
				info.Type = meta["type"]
			}
		}
		result = append(result, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	snapDir := filepath.Join(snapshotsBaseDir, name)
	if _, err := os.Stat(snapDir); err != nil {
		httpError(w, fmt.Errorf("snapshot %q not found", name), http.StatusNotFound)
		return
	}

	if err := os.RemoveAll(snapDir); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func httpError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
