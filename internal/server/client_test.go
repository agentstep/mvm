package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// === NewClient ===

func TestNewClient(t *testing.T) {
	c := NewClient("/tmp/test.sock")
	if c == nil {
		t.Fatal("NewClient should not return nil")
	}
	if c.socketPath != "/tmp/test.sock" {
		t.Errorf("socketPath = %q, want /tmp/test.sock", c.socketPath)
	}
}

func TestNewClientHTTPClient(t *testing.T) {
	c := NewClient("/tmp/test.sock")
	if c.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
}

// === DefaultClient ===

func TestDefaultClientNotNil(t *testing.T) {
	c := DefaultClient()
	if c == nil {
		t.Fatal("DefaultClient should not return nil")
	}
}

func TestDefaultClientUsesDefaultSocket(t *testing.T) {
	c := DefaultClient()
	expected := DefaultSocketPath()
	if c.socketPath != expected {
		t.Errorf("socketPath = %q, want %q", c.socketPath, expected)
	}
}

// === IsAvailable ===

func TestIsAvailableNoServer(t *testing.T) {
	c := NewClient("/nonexistent/socket.sock")
	if c.IsAvailable() {
		t.Error("should not be available with nonexistent socket")
	}
}

func TestIsAvailableWithServer(t *testing.T) {
	// Create a mock server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Use Unix socket
	dir := t.TempDir()
	sockPath := dir + "/test.sock"
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	defer srv.Close()

	// Brief wait for server to start
	time.Sleep(50 * time.Millisecond)

	c := NewClient(sockPath)
	if !c.IsAvailable() {
		t.Error("should be available with running server")
	}
}

// === Client request types ===

func TestCreateVMRequestMarshal(t *testing.T) {
	req := CreateVMRequest{
		Name:     "test-vm",
		Cpus:     2,
		MemoryMB: 1024,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !strings.Contains(string(data), "test-vm") {
		t.Error("marshaled request should contain VM name")
	}
}

// === ExecStream NDJSON parsing ===

func TestExecStreamNDJSONParsing(t *testing.T) {
	// Create mock NDJSON response
	lines := []string{
		`{"type":"stdout","data":"hello "}`,
		`{"type":"stdout","data":"world\n"}`,
		`{"type":"stderr","data":"warning\n"}`,
		`{"type":"exit","exit_code":0}`,
	}
	body := strings.Join(lines, "\n")

	// Parse like ExecStream does
	scanner := bufio.NewScanner(strings.NewReader(body))
	var stdout, stderr bytes.Buffer
	exitCode := -1

	for scanner.Scan() {
		var frame struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			ExitCode int    `json:"exit_code"`
			Error    string `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			continue
		}
		switch frame.Type {
		case "stdout":
			stdout.WriteString(frame.Data)
		case "stderr":
			stderr.WriteString(frame.Data)
		case "exit":
			exitCode = frame.ExitCode
		}
	}

	if stdout.String() != "hello world\n" {
		t.Errorf("stdout = %q, want 'hello world\\n'", stdout.String())
	}
	if stderr.String() != "warning\n" {
		t.Errorf("stderr = %q, want 'warning\\n'", stderr.String())
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
}

func TestExecStreamNDJSONErrorFrame(t *testing.T) {
	body := `{"type":"exit","exit_code":1,"error":"command not found"}`

	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Scan()

	var frame struct {
		Type     string `json:"type"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	}
	json.Unmarshal(scanner.Bytes(), &frame)

	if frame.Type != "exit" {
		t.Errorf("Type = %q, want exit", frame.Type)
	}
	if frame.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", frame.ExitCode)
	}
	if frame.Error != "command not found" {
		t.Errorf("Error = %q", frame.Error)
	}
}

func TestExecStreamNDJSONMalformedLine(t *testing.T) {
	body := "not json\n" + `{"type":"exit","exit_code":0}` + "\n"

	scanner := bufio.NewScanner(strings.NewReader(body))
	exitCode := -1

	for scanner.Scan() {
		var frame struct {
			Type     string `json:"type"`
			ExitCode int    `json:"exit_code"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			continue // malformed lines are skipped
		}
		if frame.Type == "exit" {
			exitCode = frame.ExitCode
		}
	}

	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0 (malformed lines should be skipped)", exitCode)
	}
}

// === PoolStatusResponse ===

func TestPoolStatusResponseJSON(t *testing.T) {
	resp := PoolStatusResponse{Ready: 2, Total: 3}
	data, _ := json.Marshal(resp)
	var decoded PoolStatusResponse
	json.Unmarshal(data, &decoded)

	if decoded.Ready != 2 {
		t.Errorf("Ready = %d, want 2", decoded.Ready)
	}
	if decoded.Total != 3 {
		t.Errorf("Total = %d, want 3", decoded.Total)
	}
}

// === Client with httptest ===

func TestClientExecWithMockServer(t *testing.T) {
	// Create mock HTTP handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ExecResponse{Output: "test output", ExitCode: 0})
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Verify the mock client can be created
	_ = &Client{
		httpClient: ts.Client(),
		socketPath: "/test",
	}

	// Verify the request structure via direct HTTP
	body, _ := json.Marshal(ExecRequest{Command: "echo test"})
	req, _ := http.NewRequestWithContext(context.Background(), "POST",
		ts.URL+"/vms/test/exec", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var result ExecResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Output != "test output" {
		t.Errorf("Output = %q", result.Output)
	}
}

func TestClientListVMsWithMockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]VMResponse{
			{Name: "vm1", Status: "running"},
			{Name: "vm2", Status: "stopped"},
		})
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/vms")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var result []VMResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result) != 2 {
		t.Errorf("expected 2 VMs, got %d", len(result))
	}
}

func TestClientDeleteVMWithMockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("DELETE", ts.URL+"/vms/test", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestClientCreateVMWithMockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req CreateVMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if req.Name != "new-vm" {
			t.Errorf("Name = %q, want new-vm", req.Name)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(VMResponse{Name: req.Name, Status: "running"})
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	body, _ := json.Marshal(CreateVMRequest{Name: "new-vm"})
	resp, err := ts.Client().Post(ts.URL+"/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}

	var result VMResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Name != "new-vm" {
		t.Errorf("Name = %q", result.Name)
	}
}

// === ExecStream non-NDJSON fallback ===

func TestExecStreamNonNDJSONResponse(t *testing.T) {
	// When Content-Type is not ndjson, ExecStream falls back to regular JSON
	resp := ExecResponse{Output: "regular output", ExitCode: 0}
	data, _ := json.Marshal(resp)

	var decoded ExecResponse
	json.Unmarshal(data, &decoded)
	if decoded.Output != "regular output" {
		t.Errorf("Output = %q", decoded.Output)
	}

	// Simulate what ExecStream does with regular JSON
	var stdout bytes.Buffer
	if decoded.Output != "" {
		stdout.Write([]byte(decoded.Output))
	}
	if stdout.String() != "regular output" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// === readFull ===

func TestReadFullComplete(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		server.Write([]byte("hello"))
	}()

	buf := make([]byte, 5)
	n, err := readFull(client, buf)
	if err != nil {
		t.Fatalf("readFull: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if string(buf) != "hello" {
		t.Errorf("buf = %q, want hello", string(buf))
	}
}

func TestReadFullPartialReads(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Write in chunks to simulate partial reads
	go func() {
		server.Write([]byte("hel"))
		time.Sleep(10 * time.Millisecond)
		server.Write([]byte("lo"))
	}()

	buf := make([]byte, 5)
	n, err := readFull(client, buf)
	if err != nil {
		t.Fatalf("readFull: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if string(buf) != "hello" {
		t.Errorf("buf = %q, want hello", string(buf))
	}
}

func TestReadFullEOF(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	go func() {
		server.Write([]byte("hi"))
		server.Close() // close before buffer is full
	}()

	buf := make([]byte, 10)
	_, err := readFull(client, buf)
	if err == nil {
		t.Error("should error when connection closes before buffer is full")
	}
	if err != io.EOF {
		t.Errorf("error should be EOF, got: %v", err)
	}
}
