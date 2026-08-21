package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestApplevzCreateRefusesNetPolicy is a regression guard on the most
// dangerous shape of "unsupported option".
//
// The applevz create path applies no network policy. Accepting one would
// return 201 Created for a VM with unrestricted network access — the caller
// believes it asked for a closed sandbox and holds a success response saying
// it got one. Every other unsupported option already 400s; silence here is a
// security claim rather than a missing feature.
func TestApplevzCreateRefusesNetPolicy(t *testing.T) {
	s, store := testServer(t)
	store.MarkInitialized("", "applevz")

	for _, policy := range []string{"deny", "allow:github.com"} {
		body, _ := json.Marshal(CreateVMRequest{Name: "vz1", NetPolicy: policy})
		req := httptest.NewRequest("POST", "/v1/vms", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleCreateVM(w, req)

		if w.Code != 400 {
			t.Errorf("net_policy %q returned %d, want 400 — a policy that is not "+
				"applied must never be answered with success", policy, w.Code)
		}
		if !strings.Contains(w.Body.String(), "net_policy") {
			t.Errorf("net_policy %q: error should name the field, got %q", policy, w.Body.String())
		}
	}
}

// An explicitly-open policy is genuinely supported, since "open" means no
// filter — refusing it would reject a request the backend can honour.
func TestApplevzCreateAllowsOpenPolicy(t *testing.T) {
	s, store := testServer(t)
	store.MarkInitialized("", "applevz")

	for _, policy := range []string{"", "open"} {
		body, _ := json.Marshal(CreateVMRequest{Name: "vz-open", NetPolicy: policy})
		req := httptest.NewRequest("POST", "/v1/vms", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleCreateVM(w, req)

		// It will fail later (no real VZ backend in a test), but it must not be
		// rejected as an unsupported policy.
		if w.Code == 400 && strings.Contains(w.Body.String(), "net_policy") {
			t.Errorf("policy %q was rejected as unsupported, but open means no filter", policy)
		}
	}
}

// The other unsupported options must keep 400ing rather than being silently
// dropped — that was the branch's own design rule.
func TestApplevzCreateRefusesUnsupportedOptions(t *testing.T) {
	s, store := testServer(t)
	store.MarkInitialized("", "applevz")

	cases := map[string]CreateVMRequest{
		"image":   {Name: "v", Image: "custom"},
		"seccomp": {Name: "v", Seccomp: "strict"},
		"volumes": {Name: "v", Volumes: []string{"/h:/g"}},
		"secrets": {Name: "v", Secrets: []string{"KEY"}},
	}
	for label, cr := range cases {
		body, _ := json.Marshal(cr)
		req := httptest.NewRequest("POST", "/v1/vms", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleCreateVM(w, req)
		if w.Code != 400 {
			t.Errorf("%s returned %d, want 400", label, w.Code)
		}
	}
}
