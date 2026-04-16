package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL), srv
}

func TestHealth_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("expected path /health, got %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() unexpected error: %v", err)
	}
}

func TestCreateVM_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" {
			t.Errorf("expected path /vms, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}

		var req CreateVMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Name != "test-vm" {
			t.Errorf("expected name test-vm, got %s", req.Name)
		}
		if req.Cpus != 2 {
			t.Errorf("expected cpus 2, got %d", req.Cpus)
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(VMResponse{
			Name:   "test-vm",
			Status: "running",
		})
	})

	resp, err := c.CreateVM(context.Background(), CreateVMRequest{
		Name: "test-vm",
		Cpus: 2,
	})
	if err != nil {
		t.Fatalf("CreateVM() unexpected error: %v", err)
	}
	if resp.Name != "test-vm" {
		t.Errorf("expected name test-vm, got %s", resp.Name)
	}
	if resp.Status != "running" {
		t.Errorf("expected status running, got %s", resp.Status)
	}
}

func TestExec_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms/myvm/exec" {
			t.Errorf("expected path /vms/myvm/exec, got %s", r.URL.Path)
		}

		var body struct {
			Command string `json:"command"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Command != "uname -a" {
			t.Errorf("expected command 'uname -a', got %q", body.Command)
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ExecResult{
			Output:   "Linux myvm 5.10\n",
			ExitCode: 0,
		})
	})

	result, err := c.Exec(context.Background(), "myvm", "uname -a")
	if err != nil {
		t.Fatalf("Exec() unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Output != "Linux myvm 5.10\n" {
		t.Errorf("unexpected output: %q", result.Output)
	}
}

func TestListVMs_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" {
			t.Errorf("expected path /vms, got %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]VMResponse{
			{Name: "vm1", Status: "running"},
			{Name: "vm2", Status: "stopped"},
		})
	})

	vms, err := c.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs() unexpected error: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("expected 2 VMs, got %d", len(vms))
	}
	if vms[0].Name != "vm1" || vms[1].Name != "vm2" {
		t.Errorf("unexpected VM names: %s, %s", vms[0].Name, vms[1].Name)
	}
}

func TestAuthHeader_Sent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithAPIKey("secret-token"))
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("expected 'Bearer secret-token', got %q", gotAuth)
	}
}

func TestAuthHeader_NotSent_WhenEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() unexpected error: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestErrorResponse_4xx(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "VM not found"})
	})

	err := c.DeleteVM(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "VM not found" {
		t.Errorf("expected message 'VM not found', got %q", apiErr.Message)
	}
}

func TestErrorResponse_5xx(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "internal failure"})
	})

	_, err := c.ListVMs(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", apiErr.StatusCode)
	}
}

func TestDeleteVM_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms/myvm" {
			t.Errorf("expected path /vms/myvm, got %s", r.URL.Path)
		}
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})

	if err := c.DeleteVM(context.Background(), "myvm"); err != nil {
		t.Fatalf("DeleteVM() unexpected error: %v", err)
	}
}

func TestStopVM_Force(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Force bool `json:"force"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !body.Force {
			t.Error("expected force=true")
		}
		w.WriteHeader(http.StatusOK)
	})

	if err := c.StopVM(context.Background(), "myvm", true); err != nil {
		t.Fatalf("StopVM() unexpected error: %v", err)
	}
}

func TestPoolStatus_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pool" {
			t.Errorf("expected path /pool, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(PoolStatus{Ready: 3, Total: 5})
	})

	ps, err := c.PoolStatus(context.Background())
	if err != nil {
		t.Fatalf("PoolStatus() unexpected error: %v", err)
	}
	if ps.Ready != 3 || ps.Total != 5 {
		t.Errorf("expected Ready=3 Total=5, got Ready=%d Total=%d", ps.Ready, ps.Total)
	}
}

func TestSnapshotList_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshots" {
			t.Errorf("expected path /snapshots, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]SnapshotInfo{
			{Name: "snap1", VM: "vm1"},
		})
	})

	snaps, err := c.SnapshotList(context.Background())
	if err != nil {
		t.Fatalf("SnapshotList() unexpected error: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Name != "snap1" {
		t.Errorf("unexpected snapshots: %+v", snaps)
	}
}
