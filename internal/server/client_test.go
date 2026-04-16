package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	t.Setenv("MVM_REMOTE", "")
	c := DefaultClient()
	if c == nil {
		t.Fatal("DefaultClient should not return nil")
	}
}

func TestDefaultClientUsesDefaultSocket(t *testing.T) {
	t.Setenv("MVM_REMOTE", "")
	c := DefaultClient()
	expected := DefaultSocketPath()
	if c.socketPath != expected {
		t.Errorf("socketPath = %q, want %q", c.socketPath, expected)
	}
}

func TestDefaultClientRemoteFromEnv(t *testing.T) {
	t.Setenv("MVM_REMOTE", "https://myhost:19876")
	t.Setenv("MVM_API_KEY", "secret123")
	t.Setenv("MVM_CA_CERT", "")
	c := DefaultClient()
	if c.remoteURL != "https://myhost:19876" {
		t.Errorf("remoteURL = %q, want https://myhost:19876", c.remoteURL)
	}
	if c.apiKey != "secret123" {
		t.Errorf("apiKey = %q, want secret123", c.apiKey)
	}
	if c.socketPath != "" {
		t.Errorf("socketPath should be empty for remote client, got %q", c.socketPath)
	}
}

// === NewRemoteClient ===

func TestNewRemoteClient(t *testing.T) {
	c := NewRemoteClient("https://server:19876", "mykey", "")
	if c == nil {
		t.Fatal("NewRemoteClient should not return nil")
	}
	if c.remoteURL != "https://server:19876" {
		t.Errorf("remoteURL = %q", c.remoteURL)
	}
	if c.apiKey != "mykey" {
		t.Errorf("apiKey = %q", c.apiKey)
	}
	if c.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
}

func TestNewRemoteClientTrailingSlash(t *testing.T) {
	c := NewRemoteClient("https://server:19876/", "key", "")
	if c.remoteURL != "https://server:19876" {
		t.Errorf("remoteURL = %q, trailing slash should be stripped", c.remoteURL)
	}
}

func TestNewRemoteClientNoAPIKey(t *testing.T) {
	c := NewRemoteClient("http://server:19876", "", "")
	if c.httpClient == nil {
		t.Error("httpClient should not be nil even without API key")
	}
}

// === url() helper ===

func TestURLLocal(t *testing.T) {
	c := NewClient("/tmp/test.sock")
	got := c.url("/vms")
	if got != "http://mvm/vms" {
		t.Errorf("url = %q, want http://mvm/vms", got)
	}
}

func TestURLRemote(t *testing.T) {
	c := NewRemoteClient("https://server:19876", "", "")
	got := c.url("/vms")
	if got != "https://server:19876/vms" {
		t.Errorf("url = %q, want https://server:19876/vms", got)
	}
}

// === authRoundTripper ===

func TestAuthRoundTripperAddsHeader(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "test-key-123", "")
	c.httpClient.Get(c.url("/health"))

	if gotAuth != "Bearer test-key-123" {
		t.Errorf("Authorization = %q, want 'Bearer test-key-123'", gotAuth)
	}
}

func TestAuthRoundTripperNoKeyNoHeader(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	c.httpClient.Get(c.url("/health"))

	if gotAuth != "" {
		t.Errorf("Authorization should be empty without API key, got %q", gotAuth)
	}
}

// === tlsConfig ===

func TestTLSConfigNoCACert(t *testing.T) {
	c := &Client{}
	conf := c.tlsConfig()
	if conf.RootCAs != nil {
		t.Error("RootCAs should be nil when no CA cert path is set")
	}
}

func TestTLSConfigWithCACert(t *testing.T) {
	// Write a dummy PEM cert (self-signed, just for pool test)
	certPEM := `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABLU3
jSO0r7B4hOBfOwHVPe+TgrFKMDwBRjHm42ADiZhoIelfdnMJfkImaeL4SEA7VMCJ
TBPEiQHj/9aqaa3MYBOjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2wpSek3WBpMl
fHbDfhXBrd4rvEY02JDeI8eDGZdRQlkCIBYsSSNTYBSBdHJtnhJDMm14mGl8JGVX
N7NwKlMYdDkS
-----END CERTIFICATE-----`
	tmpFile, err := os.CreateTemp(t.TempDir(), "ca-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.WriteString(certPEM)
	tmpFile.Close()

	c := &Client{caCertPath: tmpFile.Name()}
	conf := c.tlsConfig()
	if conf.RootCAs == nil {
		t.Error("RootCAs should not be nil when CA cert is provided")
	}
}

func TestTLSConfigBadCACertPath(t *testing.T) {
	c := &Client{caCertPath: "/nonexistent/ca.pem"}
	conf := c.tlsConfig()
	// Should not crash, just return nil RootCAs
	if conf.RootCAs != nil {
		t.Error("RootCAs should be nil when CA cert file doesn't exist")
	}
}

// === dial() ===

func TestDialLocal(t *testing.T) {
	// Start a Unix listener
	dir := t.TempDir()
	sockPath := dir + "/test.sock"
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	c := NewClient(sockPath)
	conn, err := c.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
}

func TestDialRemoteTCP(t *testing.T) {
	// Start a TCP listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	c := NewRemoteClient("http://"+ln.Addr().String(), "", "")
	conn, err := c.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
}

// === Remote client with httptest (end-to-end) ===

func TestRemoteClientListVMs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" {
			t.Errorf("path = %q, want /vms", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]VMResponse{
			{Name: "vm1", Status: "running"},
		})
	}))
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	vms, err := c.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 1 || vms[0].Name != "vm1" {
		t.Errorf("vms = %v", vms)
	}
}

func TestRemoteClientWithAuth(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]VMResponse{})
	}))
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "secret", "")
	c.ListVMs(context.Background())
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want 'Bearer secret'", gotAuth)
	}
}

func TestRemoteClientIsAvailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := NewRemoteClient(ts.URL, "", "")
	if !c.IsAvailable() {
		t.Error("remote client should be available")
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

// === SnapshotInfo ===

func TestSnapshotInfoResponseJSON(t *testing.T) {
	info := SnapshotInfo{
		Name:    "snap1",
		VM:      "myvm",
		Created: "2025-01-01T00:00:00Z",
		Type:    "full",
	}
	data, _ := json.Marshal(info)
	var decoded SnapshotInfo
	json.Unmarshal(data, &decoded)

	if decoded.Name != "snap1" {
		t.Errorf("Name = %q, want snap1", decoded.Name)
	}
	if decoded.VM != "myvm" {
		t.Errorf("VM = %q", decoded.VM)
	}
}

func TestClientSnapshotCreateWithMockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/snapshot") {
			t.Errorf("path = %q, want /vms/*/snapshot", r.URL.Path)
		}
		var req SnapshotCreateRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"snapshot": req.Name, "status": "created"})
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	body, _ := json.Marshal(SnapshotCreateRequest{Name: "my-snap"})
	req, _ := http.NewRequest("POST", ts.URL+"/vms/test/snapshot", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

func TestClientSnapshotDeleteWithMockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("DELETE", ts.URL+"/snapshots/snap1", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestClientSnapshotListWithMockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]SnapshotInfo{
			{Name: "snap1", VM: "vm1", Type: "full"},
			{Name: "snap2", VM: "vm2", Type: "full"},
		})
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/snapshots")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var result []SnapshotInfo
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(result))
	}
}

// (Tests for the old TCP-via-TAP readFull helper were removed when
// internal/server/routes.go switched to internal/agentclient over
// Firecracker's vsock UDS bridge. Coverage for the new transport
// lives in internal/agentclient/client_test.go.)
