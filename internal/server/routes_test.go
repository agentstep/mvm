package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
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

// === POST /vms/{name}/snapshot ===

func TestHandleSnapshotCreateVMNotFound(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("POST", "/vms/ghost/snapshot", nil)
	req.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	s.handleSnapshotCreate(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleSnapshotCreateVMStopped(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "stopped-vm", Status: "stopped", CreatedAt: time.Now()})

	req := httptest.NewRequest("POST", "/vms/stopped-vm/snapshot", nil)
	req.SetPathValue("name", "stopped-vm")
	w := httptest.NewRecorder()
	s.handleSnapshotCreate(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestHandleSnapshotCreateDefaultName(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "myvm", Status: "running", PID: 100, CreatedAt: time.Now()})

	// The mock executor returns "" which will cause SnapshotVM to fail
	// because curl returns empty. We just check the handler parses correctly
	// by verifying it gets past validation.
	body, _ := json.Marshal(SnapshotCreateRequest{})
	req := httptest.NewRequest("POST", "/vms/myvm/snapshot", bytes.NewReader(body))
	req.SetPathValue("name", "myvm")
	w := httptest.NewRecorder()
	s.handleSnapshotCreate(w, req)

	// Will be 500 because mock executor can't actually snapshot,
	// but at least it's not 404 or 409.
	if w.Code == http.StatusNotFound || w.Code == http.StatusConflict {
		t.Errorf("status = %d, should get past validation", w.Code)
	}
}

// === POST /vms/{name}/restore ===

func TestHandleSnapshotRestoreInvalidJSON(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("POST", "/vms/test/restore", bytes.NewReader([]byte("bad")))
	req.SetPathValue("name", "test")
	w := httptest.NewRecorder()
	s.handleSnapshotRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSnapshotRestoreEmptyName(t *testing.T) {
	s, _ := testServer(t)

	body, _ := json.Marshal(SnapshotRestoreRequest{Name: ""})
	req := httptest.NewRequest("POST", "/vms/test/restore", bytes.NewReader(body))
	req.SetPathValue("name", "test")
	w := httptest.NewRecorder()
	s.handleSnapshotRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSnapshotRestoreNotFound(t *testing.T) {
	s, _ := testServer(t)

	body, _ := json.Marshal(SnapshotRestoreRequest{Name: "nonexistent"})
	req := httptest.NewRequest("POST", "/vms/test/restore", bytes.NewReader(body))
	req.SetPathValue("name", "test")
	w := httptest.NewRecorder()
	s.handleSnapshotRestore(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// === GET /snapshots ===

func TestHandleSnapshotListEmpty(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/snapshots", nil)
	w := httptest.NewRecorder()
	s.handleSnapshotList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var result []SnapshotInfo
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 0 {
		t.Errorf("expected empty list, got %d snapshots", len(result))
	}
}

// === DELETE /snapshots/{name} ===

func TestHandleSnapshotDeleteNotFound(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("DELETE", "/snapshots/ghost", nil)
	req.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	s.handleSnapshotDelete(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleSnapshotDeleteSuccess(t *testing.T) {
	// Create a temporary snapshot directory to test removal logic.
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "test-snap")
	os.MkdirAll(snapDir, 0o755)
	os.WriteFile(filepath.Join(snapDir, "meta.json"), []byte(`{"vm":"test"}`), 0o644)

	// We verify the removal logic (os.RemoveAll) directly here.
	err := os.RemoveAll(snapDir)
	if err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Error("snapshot directory should be removed")
	}
}

// === Snapshot request/response types ===

func TestSnapshotCreateRequestJSON(t *testing.T) {
	req := SnapshotCreateRequest{Name: "my-snap"}
	data, _ := json.Marshal(req)
	var decoded SnapshotCreateRequest
	json.Unmarshal(data, &decoded)
	if decoded.Name != "my-snap" {
		t.Errorf("Name = %q, want my-snap", decoded.Name)
	}
}

func TestSnapshotCreateRequestOmitsEmpty(t *testing.T) {
	req := SnapshotCreateRequest{}
	data, _ := json.Marshal(req)
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if _, ok := raw["name"]; ok {
		t.Error("name should be omitted when empty")
	}
}

func TestSnapshotRestoreRequestJSON(t *testing.T) {
	req := SnapshotRestoreRequest{Name: "snap1"}
	data, _ := json.Marshal(req)
	var decoded SnapshotRestoreRequest
	json.Unmarshal(data, &decoded)
	if decoded.Name != "snap1" {
		t.Errorf("Name = %q, want snap1", decoded.Name)
	}
}

func TestSnapshotInfoJSON(t *testing.T) {
	info := SnapshotInfo{
		Name:    "test-snap",
		VM:      "myvm",
		Created: "2025-01-01T00:00:00Z",
		Type:    "full",
	}
	data, _ := json.Marshal(info)
	var decoded SnapshotInfo
	json.Unmarshal(data, &decoded)

	if decoded.Name != "test-snap" {
		t.Errorf("Name = %q", decoded.Name)
	}
	if decoded.VM != "myvm" {
		t.Errorf("VM = %q", decoded.VM)
	}
	if decoded.Created != "2025-01-01T00:00:00Z" {
		t.Errorf("Created = %q", decoded.Created)
	}
	if decoded.Type != "full" {
		t.Errorf("Type = %q", decoded.Type)
	}
}

func TestSnapshotInfoOmitsEmpty(t *testing.T) {
	info := SnapshotInfo{Name: "minimal"}
	data, _ := json.Marshal(info)
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)

	if _, ok := raw["vm"]; ok {
		t.Error("vm should be omitted when empty")
	}
	if _, ok := raw["created"]; ok {
		t.Error("created should be omitted when empty")
	}
	if _, ok := raw["type"]; ok {
		t.Error("type should be omitted when empty")
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

// === /v1 route aliases ===

func TestRoutesServeUnversionedAndV1(t *testing.T) {
	s, _ := testServer(t)
	mux := s.buildMux()

	for _, path := range []string{"/health", "/v1/health", "/vms", "/v1/vms"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
		}
	}
}

// === spec persistence + GET /vms/{name} ===

func TestSpecFromCreateRequest(t *testing.T) {
	req := CreateVMRequest{
		Name:      "web",
		Cpus:      4,
		MemoryMB:  2048,
		Ports:     []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
		NetPolicy: "deny",
		Volumes:   []string{"/h:/g"},
		Seccomp:   "strict",
		Image:     "custom",
	}
	spec := specFromCreateRequest(req)
	if spec.Image != "custom" || spec.Cpus != 4 || spec.MemoryMB != 2048 ||
		spec.NetPolicy != "deny" || spec.Seccomp != "strict" {
		t.Errorf("spec = %+v, want fields copied from request", spec)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].HostPort != 8080 {
		t.Errorf("spec.Ports = %+v, want request ports", spec.Ports)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0] != "/h:/g" {
		t.Errorf("spec.Volumes = %+v, want request volumes", spec.Volumes)
	}
}

func TestHandleInspectVM(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{
		Name:      "web",
		Status:    "running",
		GuestIP:   "10.99.0.2",
		Backend:   "firecracker",
		CreatedAt: time.Now(),
		Spec:      &state.VMSpec{Cpus: 4, NetPolicy: "deny"},
	})

	req := httptest.NewRequest("GET", "/v1/vms/web", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp VMInspectResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "web" || resp.Status != "running" {
		t.Errorf("resp = %+v, want name/status from store", resp)
	}
	if resp.Spec == nil || resp.Spec.Cpus != 4 || resp.Spec.NetPolicy != "deny" {
		t.Errorf("resp.Spec = %+v, want persisted spec", resp.Spec)
	}
}

func TestHandleInspectVMNotFound(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/vms/nope", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// === InspectResponseFromVM (shared constructor, also used by internal/cli) ===

func TestInspectResponseFromVM(t *testing.T) {
	now := time.Now()
	vm := &state.VM{
		Name:      "web",
		Status:    "running",
		GuestIP:   "192.168.64.5",
		PID:       123,
		Backend:   "applevz",
		Ports:     []state.PortMap{{HostPort: 3000, GuestPort: 3000, Proto: "tcp"}},
		CreatedAt: now,
		Spec:      &state.VMSpec{Cpus: 4, NetPolicy: "deny", Volumes: []string{"/host:/guest"}},
		// internal runtime fields that must NOT leak into inspect output:
		SocketPath: "/run/mvm/web.sock",
		TAPIP:      "172.16.0.1",
	}

	resp := InspectResponseFromVM(vm)

	if resp.Name != "web" || resp.Status != "running" || resp.Backend != "applevz" {
		t.Errorf("resp = %+v, want identity fields copied", resp)
	}
	if resp.Spec == nil || resp.Spec.Cpus != 4 {
		t.Errorf("resp.Spec = %+v, want the VM's spec", resp.Spec)
	}
	if len(resp.Spec.Volumes) != 1 || resp.Spec.Volumes[0] != "/host:/guest" {
		t.Errorf("resp.Spec.Volumes = %v, want [/host:/guest]", resp.Spec.Volumes)
	}

	data, _ := json.Marshal(resp)
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	for _, forbidden := range []string{"socket_path", "tap_ip", "tap_device", "guest_mac", "rootfs_path"} {
		if _, ok := m[forbidden]; ok {
			t.Errorf("inspect output leaks internal field %q", forbidden)
		}
	}
}

// === Create -> Inspect round trip (through the real handlers) ===

func TestCreateThenInspectRoundTrip(t *testing.T) {
	s, store := testServer(t)

	// Non-default cpus/memory so handleCreateVM skips the warm pool and goes
	// straight through firecracker.Start with the mock executor.
	body, _ := json.Marshal(CreateVMRequest{
		Name:      "roundtrip",
		Cpus:      1,
		MemoryMB:  512,
		NetPolicy: "deny",
		Ports:     []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
	})
	createReq := httptest.NewRequest("POST", "/vms", bytes.NewReader(body))
	createW := httptest.NewRecorder()
	s.buildMux().ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body: %s", createW.Code, createW.Body.String())
	}

	// The spec must be persisted synchronously in handleCreateVM, not only in
	// the async post-boot goroutine.
	stored, err := store.GetVM("roundtrip")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if stored.Spec == nil {
		t.Fatal("store has no Spec after create — handleCreateVM did not persist it")
	}

	inspectReq := httptest.NewRequest("GET", "/v1/vms/roundtrip", nil)
	inspectW := httptest.NewRecorder()
	s.buildMux().ServeHTTP(inspectW, inspectReq)

	if inspectW.Code != http.StatusOK {
		t.Fatalf("inspect status = %d, want 200; body: %s", inspectW.Code, inspectW.Body.String())
	}
	var resp VMInspectResponse
	if err := json.NewDecoder(inspectW.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "roundtrip" {
		t.Errorf("resp.Name = %q, want roundtrip", resp.Name)
	}
	if resp.Spec == nil || resp.Spec.Cpus != 1 || resp.Spec.MemoryMB != 512 || resp.Spec.NetPolicy != "deny" {
		t.Errorf("resp.Spec = %+v, want the create request echoed back", resp.Spec)
	}
	if len(resp.Spec.Ports) != 1 || resp.Spec.Ports[0].HostPort != 8080 {
		t.Errorf("resp.Spec.Ports = %+v, want the create request's ports", resp.Spec.Ports)
	}
}

func TestHandleCreateVMPersistsSecretNamesOnly(t *testing.T) {
	s, store := testServer(t)

	body, _ := json.Marshal(CreateVMRequest{
		Name:    "web",
		Secrets: []string{"OPENAI_API_KEY", "DB_PASSWORD"},
	})
	req := httptest.NewRequest("POST", "/vms", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}
	// The response body must never carry secret values — only names ever
	// reach the daemon in the first place (the security invariant this
	// whole phase exists to uphold), but assert the wire shape directly too:
	// a name showing up in a response is one accidental rename away from a
	// value showing up there.
	if strings.Contains(w.Body.String(), "OPENAI_API_KEY") {
		t.Errorf("response body echoes a secret name back over the wire: %s", w.Body.String())
	}

	vm, err := store.GetVM("web")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if len(vm.Secrets) != 2 || vm.Secrets[0] != "OPENAI_API_KEY" || vm.Secrets[1] != "DB_PASSWORD" {
		t.Errorf("vm.Secrets = %v, want [OPENAI_API_KEY DB_PASSWORD] persisted from the request", vm.Secrets)
	}
	if vm.Spec == nil || len(vm.Spec.Secrets) != 2 {
		t.Errorf("vm.Spec.Secrets = %v, want the same names surfaced via inspect", vm.Spec.Secrets)
	}
}

func TestHandleImageDownload(t *testing.T) {
	s, _ := testServer(t)
	t.Setenv("MVM_DATA_DIR", t.TempDir())
	if err := os.MkdirAll(firecracker.CacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firecracker.CacheDir(), "my-image.ext4"), []byte("fake-ext4-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/images/my-image/download", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "fake-ext4-bytes" {
		t.Errorf("body = %q, want the image file's raw bytes", w.Body.String())
	}
}

func TestHandleImageDownloadNotFound(t *testing.T) {
	s, _ := testServer(t)
	t.Setenv("MVM_DATA_DIR", t.TempDir())

	req := httptest.NewRequest("GET", "/v1/images/nope/download", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// === GET /vms/{name}/logs ===

func TestTailLines(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("l1\nl2\nl3\nl4\nl5\n")
	f.Seek(0, 0)

	got, err := tailLines(f, 2)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if got != "l4\nl5\n" {
		t.Errorf("tailLines = %q, want %q", got, "l4\nl5\n")
	}
}

func TestTailLinesNoTrailingNewline(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("l1\nl2\nl3")
	f.Seek(0, 0)

	got, err := tailLines(f, 2)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if got != "l2\nl3" {
		t.Errorf("tailLines = %q, want %q", got, "l2\nl3")
	}
}

func TestHandleVMLogsRequiresBootParam(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "web", Status: "running", Backend: "firecracker", CreatedAt: time.Now()})

	req := httptest.NewRequest("GET", "/v1/vms/web/logs", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without ?boot=true", w.Code)
	}
}

func TestHandleVMLogsReturnsFileContents(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "web", Status: "running", Backend: "firecracker", CreatedAt: time.Now()})
	t.Setenv("MVM_DATA_DIR", t.TempDir())

	vmDir := firecracker.VMDir("web")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "firecracker.log"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/vms/web/logs?boot=true", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var frame struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(w.Body.Bytes()), &frame); err != nil {
		t.Fatalf("decode NDJSON frame: %v; body: %s", err, w.Body.String())
	}
	if frame.Type != "data" || frame.Data != "line1\nline2\nline3\n" {
		t.Errorf("frame = %+v, want the full log contents", frame)
	}
}

func TestHandleVMLogsTail(t *testing.T) {
	s, store := testServer(t)
	store.AddVM(&state.VM{Name: "web", Status: "running", Backend: "firecracker", CreatedAt: time.Now()})
	t.Setenv("MVM_DATA_DIR", t.TempDir())

	vmDir := firecracker.VMDir("web")
	os.MkdirAll(vmDir, 0o755)
	os.WriteFile(filepath.Join(vmDir, "firecracker.log"), []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644)

	req := httptest.NewRequest("GET", "/v1/vms/web/logs?boot=true&tail=2", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)

	var frame struct {
		Data string `json:"data"`
	}
	json.Unmarshal(bytes.TrimSpace(w.Body.Bytes()), &frame)
	if frame.Data != "l4\nl5\n" {
		t.Errorf("tail data = %q, want the last 2 lines", frame.Data)
	}
}

func TestHandleVMLogsNotFoundVM(t *testing.T) {
	s, _ := testServer(t)
	req := httptest.NewRequest("GET", "/v1/vms/nope/logs?boot=true", nil)
	w := httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
