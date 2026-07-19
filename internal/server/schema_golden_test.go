package server

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/state"
)

// These goldens enforce the design-spec guardrail (2026-07-19 image/VM
// organization): JSON schemas are additive-only. Adding a key means adding
// it to `want` here — a deliberate act. Removing or renaming a key breaks
// Gateway and SDK consumers and is forbidden by the deprecation policy.

func jsonKeys(t *testing.T, v interface{}) []string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fullVMResponse sets every field so no key is dropped by omitempty.
func fullVMResponse() VMResponse {
	return VMResponse{
		Name:      "vm",
		Status:    "running",
		GuestIP:   "10.0.0.2",
		PID:       1,
		Backend:   "firecracker",
		Ports:     []state.PortMap{{HostPort: 1, GuestPort: 2, Proto: "tcp"}},
		CreatedAt: time.Now(),
		Error:     "e",
	}
}

func TestVMResponseSchemaGolden(t *testing.T) {
	want := []string{"backend", "created_at", "error", "guest_ip", "name", "pid", "ports", "status"}
	if got := jsonKeys(t, fullVMResponse()); !reflect.DeepEqual(got, want) {
		t.Errorf("VMResponse keys = %v, want %v (additive-only: update want when adding; never remove/rename)", got, want)
	}
}

func TestVMInspectResponseSchemaGolden(t *testing.T) {
	full := VMInspectResponse{
		VMResponse: fullVMResponse(),
		Spec: &state.VMSpec{
			Image:     "i",
			Cpus:      1,
			MemoryMB:  1,
			Ports:     []state.PortMap{{HostPort: 1, GuestPort: 2, Proto: "tcp"}},
			Volumes:   []string{"v"},
			NetPolicy: "open",
			Seccomp:   "strict",
			Secrets:   []string{"s"},
			Startup:   json.RawMessage(`{}`),
		},
	}
	want := []string{"backend", "created_at", "error", "guest_ip", "name", "pid", "ports", "spec", "status"}
	if got := jsonKeys(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("VMInspectResponse keys = %v, want %v", got, want)
	}

	specWant := []string{"cpus", "image", "memory_mb", "net_policy", "ports", "seccomp", "secrets", "startup", "volumes"}
	if got := jsonKeys(t, full.Spec); !reflect.DeepEqual(got, specWant) {
		t.Errorf("VMSpec keys = %v, want %v", got, specWant)
	}
}

func TestVMStatsSchemaGolden(t *testing.T) {
	full := VMStats{
		Name:    "vm",
		Backend: "firecracker",
		PID:     1,
		CPUPct:  1.5,
		MemMB:   256,
		Status:  "running",
		Error:   "e",
	}
	want := []string{"backend", "cpu_pct", "error", "mem_mb", "name", "pid", "status"}
	if got := jsonKeys(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("VMStats keys = %v, want %v (additive-only: update want when adding; never remove/rename)", got, want)
	}
}
