package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func mustReadSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// The tiering thresholds must be settable through the API, or the whole feature is dead code.
//
// state.VM has carried IdleTimeout and ArchiveAfter, and the sweep has read them, since idle
// tiering shipped — but nothing ever wrote them. No request field, no update route, no CLI flag.
// The sweep ran every interval and every VM opted out.
func TestCreateAcceptsTieringThresholds(t *testing.T) {
	spec := specFromCreateRequest(CreateVMRequest{
		Name: "vm1", IdleTimeout: "5m", ArchiveAfter: "1h",
	})
	if spec.IdleTimeout != "5m" || spec.ArchiveAfter != "1h" {
		t.Fatalf("thresholds dropped from the spec: idle=%q archive=%q", spec.IdleTimeout, spec.ArchiveAfter)
	}

	// And on the record the sweep actually reads.
	src := mustReadSource(t, "routes.go")
	for _, assign := range []string{"IdleTimeout:  req.IdleTimeout", "ArchiveAfter: req.ArchiveAfter"} {
		if strings.Count(src, assign) < 2 {
			t.Errorf("%q should appear on both the spec and the live VM record — the sweep reads the record, not the spec", assign)
		}
	}
}

// A threshold that does not parse is discarded silently by parseThreshold, which is correct for the
// sweep (a VM nobody opted in must not be archived) and wrong for create: the caller would get a
// 201 for cost control that was thrown away. Reject at the door instead.
func TestCreateRejectsUnparseableThresholds(t *testing.T) {
	s, store := testServer(t)
	store.MarkInitialized("", "applevz")

	for _, tc := range []struct{ field, value string }{
		{"idle_timeout", "5min"},
		{"idle_timeout", "soon"},
		{"idle_timeout", "-5m"},
		{"idle_timeout", "0s"},
		{"archive_after", "1 hour"},
		{"archive_after", "-1h"},
	} {
		req := CreateVMRequest{Name: "vm1"}
		if tc.field == "idle_timeout" {
			req.IdleTimeout = tc.value
		} else {
			req.ArchiveAfter = tc.value
		}
		body, _ := json.Marshal(req)
		r := httptest.NewRequest("POST", "/v1/vms", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleCreateVM(w, r)

		if w.Code != 400 {
			t.Errorf("%s=%q returned %d, want 400 — a discarded threshold must not be answered with success",
				tc.field, tc.value, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.field) {
			t.Errorf("%s=%q: error should name the field, got %q", tc.field, tc.value, w.Body.String())
		}
	}
}

// The sweep only walks Firecracker VMs, so a threshold on an applevz VM would never fire. Same
// reasoning as the net_policy refusal on that path: accepting it is a promise the daemon does not
// keep, and the caller has a 201 saying otherwise.
func TestApplevzCreateRefusesTieringThresholds(t *testing.T) {
	s, store := testServer(t)
	store.MarkInitialized("", "applevz")

	for _, req := range []CreateVMRequest{
		{Name: "vz1", IdleTimeout: "5m"},
		{Name: "vz2", ArchiveAfter: "1h"},
	} {
		body, _ := json.Marshal(req)
		r := httptest.NewRequest("POST", "/v1/vms", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleCreateVM(w, r)

		if w.Code != 400 {
			t.Errorf("applevz create with a threshold returned %d, want 400", w.Code)
		}
	}
}
