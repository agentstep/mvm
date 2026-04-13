package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// mockExecutor implements firecracker.Executor for testing.
type mockExecutor struct {
	runFunc func(command string) (string, error)
}

func (m *mockExecutor) Run(command string) (string, error) {
	if m.runFunc != nil {
		return m.runFunc(command)
	}
	return "", nil
}

func (m *mockExecutor) RunWithTimeout(command string, timeout time.Duration) (string, error) {
	return m.Run(command)
}

func testServer(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")

	ex := &mockExecutor{
		runFunc: func(command string) (string, error) {
			return "", nil
		},
	}

	s := &Server{
		store:    store,
		executor: ex,
	}
	return s, store
}

// === GET /health ===

func TestHandleHealth(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("status = %q, want ok", result["status"])
	}
}

func TestHandleHealthContentType(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// === GET /vms ===

func TestHandleListVMsEmpty(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/vms", nil)
	w := httptest.NewRecorder()
	s.handleListVMs(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var result []VMResponse
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 0 {
		t.Errorf("expected empty list, got %d VMs", len(result))
	}
}

func TestHandleListVMsWithVMs(t *testing.T) {
	s, store := testServer(t)

	store.AddVM(&state.VM{Name: "vm1", Status: "running", GuestIP: "172.16.0.2", PID: 100, CreatedAt: time.Now()})
	store.AddVM(&state.VM{Name: "vm2", Status: "stopped", GuestIP: "172.16.0.6", PID: 0, CreatedAt: time.Now()})

	req := httptest.NewRequest("GET", "/vms", nil)
	w := httptest.NewRecorder()
	s.handleListVMs(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var result []VMResponse
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 2 {
		t.Errorf("expected 2 VMs, got %d", len(result))
	}
}

func TestHandleListVMsReturnsCorrectFields(t *testing.T) {
	s, store := testServer(t)

	now := time.Now().Truncate(time.Second)
	store.AddVM(&state.VM{
		Name:      "test-fields",
		Status:    "running",
		GuestIP:   "172.16.0.2",
		PID:       1234,
		Backend:   "firecracker",
		Ports:     []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
		CreatedAt: now,
	})

	req := httptest.NewRequest("GET", "/vms", nil)
	w := httptest.NewRecorder()
	s.handleListVMs(w, req)

	var result []VMResponse
	json.NewDecoder(w.Body).Decode(&result)

	if len(result) != 1 {
		t.Fatalf("expected 1 VM, got %d", len(result))
	}
	vm := result[0]
	if vm.Name != "test-fields" {
		t.Errorf("Name = %q", vm.Name)
	}
	if vm.Status != "running" {
		t.Errorf("Status = %q", vm.Status)
	}
	if vm.GuestIP != "172.16.0.2" {
		t.Errorf("GuestIP = %q", vm.GuestIP)
	}
	if vm.PID != 1234 {
		t.Errorf("PID = %d", vm.PID)
	}
	if vm.Backend != "firecracker" {
		t.Errorf("Backend = %q", vm.Backend)
	}
	if len(vm.Ports) != 1 || vm.Ports[0].HostPort != 8080 {
		t.Errorf("Ports = %+v", vm.Ports)
	}
}

// === POST /vms — validation ===

func TestHandleCreateVMInvalidJSON(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("POST", "/vms", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	s.handleCreateVM(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleCreateVMEmptyName(t *testing.T) {
	s, _ := testServer(t)

	body, _ := json.Marshal(CreateVMRequest{Name: ""})
	req := httptest.NewRequest("POST", "/vms", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleCreateVM(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty name", w.Code)
	}
}

func TestHandleCreateVMInjectionName(t *testing.T) {
	s, _ := testServer(t)

	body, _ := json.Marshal(CreateVMRequest{Name: "vm; rm -rf /"})
	req := httptest.NewRequest("POST", "/vms", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleCreateVM(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for injection name", w.Code)
	}
}

func TestHandleCreateVMDuplicateName(t *testing.T) {
	s, store := testServer(t)

	store.AddVM(&state.VM{Name: "existing", Status: "running", CreatedAt: time.Now()})

	body, _ := json.Marshal(CreateVMRequest{Name: "existing"})
	req := httptest.NewRequest("POST", "/vms", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleCreateVM(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for duplicate name", w.Code)
	}
}

// === POST /vms/{name}/exec ===

func TestHandleExecInvalidJSON(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("POST", "/vms/test/exec", bytes.NewReader([]byte("bad json")))
	req.SetPathValue("name", "test")
	w := httptest.NewRecorder()
	s.handleExec(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleExecVMNotFound(t *testing.T) {
	s, _ := testServer(t)

	body, _ := json.Marshal(ExecRequest{Command: "echo hello"})
	req := httptest.NewRequest("POST", "/vms/nonexistent/exec", bytes.NewReader(body))
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	s.handleExec(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleExecVMNotRunning(t *testing.T) {
	s, store := testServer(t)

	store.AddVM(&state.VM{Name: "stopped-vm", Status: "stopped", CreatedAt: time.Now()})

	body, _ := json.Marshal(ExecRequest{Command: "echo hello"})
	req := httptest.NewRequest("POST", "/vms/stopped-vm/exec", bytes.NewReader(body))
	req.SetPathValue("name", "stopped-vm")
	w := httptest.NewRecorder()
	s.handleExec(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for stopped VM", w.Code)
	}
}

// === DELETE /vms/{name} ===

func TestHandleDeleteVMNotFound(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("DELETE", "/vms/ghost", nil)
	req.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	s.handleDeleteVM(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleDeleteVMSuccess(t *testing.T) {
	s, store := testServer(t)

	store.AddVM(&state.VM{Name: "todelete", Status: "running", PID: 1, CreatedAt: time.Now()})

	req := httptest.NewRequest("DELETE", "/vms/todelete", nil)
	req.SetPathValue("name", "todelete")
	w := httptest.NewRecorder()
	s.handleDeleteVM(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}

	// Verify VM is removed
	_, err := store.GetVM("todelete")
	if err == nil {
		t.Error("VM should be removed after delete")
	}
}

// === POST /vms/{name}/stop ===

func TestHandleStopVMNotFound(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("POST", "/vms/ghost/stop", nil)
	req.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	s.handleStopVM(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleStopVMSuccess(t *testing.T) {
	s, store := testServer(t)

	store.AddVM(&state.VM{Name: "tostop", Status: "running", GuestIP: "172.16.0.2", PID: 99999, CreatedAt: time.Now()})

	req := httptest.NewRequest("POST", "/vms/tostop/stop", nil)
	req.SetPathValue("name", "tostop")
	w := httptest.NewRecorder()
	s.handleStopVM(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}

	// Verify status changed
	vm, _ := store.GetVM("tostop")
	if vm.Status != "stopped" {
		t.Errorf("Status = %q, want stopped", vm.Status)
	}
	if vm.StoppedAt == nil {
		t.Error("StoppedAt should be set")
	}
}

// === GET /pool ===

func TestHandlePoolStatus(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/pool", nil)
	w := httptest.NewRecorder()
	s.handlePoolStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var result map[string]int
	json.NewDecoder(w.Body).Decode(&result)
	if _, ok := result["ready"]; !ok {
		t.Error("response should contain 'ready' field")
	}
	if _, ok := result["total"]; !ok {
		t.Error("response should contain 'total' field")
	}
}

// === POST /pool/warm ===

func TestHandlePoolWarm(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("POST", "/pool/warm", nil)
	w := httptest.NewRecorder()
	s.handlePoolWarm(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["status"] != "warming" {
		t.Errorf("status = %q, want warming", result["status"])
	}
}

// === httpError helper ===

func TestHttpError(t *testing.T) {
	w := httptest.NewRecorder()
	httpError(w, http.ErrBodyNotAllowed, http.StatusBadRequest)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["error"] == "" {
		t.Error("error message should not be empty")
	}
}

// === Request/Response types JSON marshaling ===

func TestCreateVMRequestJSON(t *testing.T) {
	req := CreateVMRequest{
		Name:      "test",
		Cpus:      4,
		MemoryMB:  2048,
		Ports:     []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
		NetPolicy: "deny",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded CreateVMRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Name != "test" {
		t.Errorf("Name = %q", decoded.Name)
	}
	if decoded.Cpus != 4 {
		t.Errorf("Cpus = %d", decoded.Cpus)
	}
	if decoded.MemoryMB != 2048 {
		t.Errorf("MemoryMB = %d", decoded.MemoryMB)
	}
	if len(decoded.Ports) != 1 {
		t.Errorf("Ports = %+v", decoded.Ports)
	}
	if decoded.NetPolicy != "deny" {
		t.Errorf("NetPolicy = %q", decoded.NetPolicy)
	}
}

func TestExecRequestJSON(t *testing.T) {
	req := ExecRequest{
		Command: "echo hello",
		Stdin:   "input data",
		Stream:  true,
	}

	data, _ := json.Marshal(req)
	var decoded ExecRequest
	json.Unmarshal(data, &decoded)

	if decoded.Command != "echo hello" {
		t.Errorf("Command = %q", decoded.Command)
	}
	if decoded.Stdin != "input data" {
		t.Errorf("Stdin = %q", decoded.Stdin)
	}
	if !decoded.Stream {
		t.Error("Stream should be true")
	}
}

func TestExecResponseJSON(t *testing.T) {
	resp := ExecResponse{
		Output:   "hello world",
		ExitCode: 0,
	}

	data, _ := json.Marshal(resp)
	var decoded ExecResponse
	json.Unmarshal(data, &decoded)

	if decoded.Output != "hello world" {
		t.Errorf("Output = %q", decoded.Output)
	}
	if decoded.ExitCode != 0 {
		t.Errorf("ExitCode = %d", decoded.ExitCode)
	}
}

func TestExecResponseWithError(t *testing.T) {
	resp := ExecResponse{
		ExitCode: 1,
		Error:    "command failed",
	}

	data, _ := json.Marshal(resp)
	var decoded ExecResponse
	json.Unmarshal(data, &decoded)

	if decoded.Error != "command failed" {
		t.Errorf("Error = %q", decoded.Error)
	}
	if decoded.ExitCode != 1 {
		t.Errorf("ExitCode = %d", decoded.ExitCode)
	}
}

func TestVMResponseJSON(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	resp := VMResponse{
		Name:      "test",
		Status:    "running",
		GuestIP:   "172.16.0.2",
		PID:       42,
		Backend:   "firecracker",
		CreatedAt: now,
	}

	data, _ := json.Marshal(resp)
	var decoded VMResponse
	json.Unmarshal(data, &decoded)

	if decoded.Name != "test" {
		t.Errorf("Name = %q", decoded.Name)
	}
	if decoded.PID != 42 {
		t.Errorf("PID = %d", decoded.PID)
	}
}

func TestCreateVMRequestOmitsZeroValues(t *testing.T) {
	req := CreateVMRequest{Name: "minimal"}
	data, _ := json.Marshal(req)

	// cpus, memory_mb should be omitted when 0
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)

	if _, ok := raw["cpus"]; ok {
		t.Error("cpus should be omitted when 0")
	}
	if _, ok := raw["memory_mb"]; ok {
		t.Error("memory_mb should be omitted when 0")
	}
}
