package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
)

// --- Request/Response types ---

type CreateVMRequest struct {
	Name      string          `json:"name"`
	Cpus      int             `json:"cpus,omitempty"`
	MemoryMB  int             `json:"memory_mb,omitempty"`
	Ports     []state.PortMap `json:"ports,omitempty"`
	NetPolicy string          `json:"net_policy,omitempty"`
}

type ExecRequest struct {
	Command string `json:"command"`
	Stdin   string `json:"stdin,omitempty"`
	Stream  bool   `json:"stream,omitempty"`
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
		if firecracker.WaitForGuest(s.executor, alloc.GuestIP, 120*time.Second) {
			firecracker.SetupGuestNetworkViaAgent(s.executor, alloc.GuestIP, alloc.TAPIP)
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
	if vm.Status != "running" {
		httpError(w, fmt.Errorf("VM %q is %s", name, vm.Status), http.StatusConflict)
		return
	}

	now := time.Now()
	s.store.UpdateVM(name, func(v *state.VM) { v.LastActivity = &now })

	// Direct TCP to guest agent (daemon runs inside Lima — same network)
	out, exitCode, execErr := execOnGuest(vm.GuestIP, req.Command, req.Stdin)
	if execErr != nil {
		httpError(w, execErr, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ExecResponse{Output: out, ExitCode: exitCode})
}

// execOnGuest sends an exec request to the guest agent via direct TCP.
func execOnGuest(guestIP, command, stdin string) (string, int, error) {
	conn, err := net.DialTimeout("tcp", guestIP+":5123", 5*time.Second)
	if err != nil {
		return "", -1, fmt.Errorf("agent not reachable at %s:5123: %w", guestIP, err)
	}
	defer conn.Close()

	// Build request
	reqJSON := fmt.Sprintf(`{"type":"exec","id":"e","exec":{"command":%q,"stdin":%q}}`, command, stdin)
	reqBytes := []byte(reqJSON)

	// Write length-prefixed request
	length := make([]byte, 4)
	length[0] = byte(len(reqBytes) >> 24)
	length[1] = byte(len(reqBytes) >> 16)
	length[2] = byte(len(reqBytes) >> 8)
	length[3] = byte(len(reqBytes))
	conn.Write(length)
	conn.Write(reqBytes)

	// Read length-prefixed response
	conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	respLen := make([]byte, 4)
	if _, err := readFull(conn, respLen); err != nil {
		return "", -1, fmt.Errorf("read response length: %w", err)
	}
	size := int(respLen[0])<<24 | int(respLen[1])<<16 | int(respLen[2])<<8 | int(respLen[3])
	respData := make([]byte, size)
	if _, err := readFull(conn, respData); err != nil {
		return "", -1, fmt.Errorf("read response: %w", err)
	}

	var resp struct {
		Data     []byte `json:"data"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(respData, &resp); err != nil {
		return "", -1, fmt.Errorf("parse response: %w", err)
	}
	if resp.Error != "" {
		return "", -1, fmt.Errorf("agent: %s", resp.Error)
	}
	return string(resp.Data), resp.ExitCode, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		nn, err := conn.Read(buf[n:])
		n += nn
		if err != nil {
			return n, err
		}
	}
	return n, nil
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

func (s *Server) handleStopVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	if vm.Status == "running" || vm.Status == "paused" {
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

func httpError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
