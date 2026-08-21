package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
)

// ServiceRequest is the body for the service routes.
type ServiceRequest struct {
	Name    string            `json:"name,omitempty"`
	Run     string            `json:"run,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Restart string            `json:"restart,omitempty"`
	Tail    int               `json:"tail,omitempty"`
}

// guestAgent returns a client for a running VM's in-guest agent, or an error
// suitable for returning to the caller.
//
// Every verb that reaches into the guest goes through here rather than dialling
// ad hoc, so the running-state check and the vsock path are stated once.
func (s *Server) guestAgent(name string) (*agentclient.Client, int, error) {
	vm, err := s.store.GetVM(name)
	if err != nil {
		return nil, http.StatusNotFound, err
	}
	if vm.Status != "running" {
		return nil, http.StatusConflict, fmt.Errorf("VM %q is %s", name, vm.Status)
	}
	return agentclient.New(&agentclient.FirecrackerVsockDialer{
		UDSPath: firecracker.VsockUDSPath(name),
	}), 0, nil
}

// handleBounce restarts user code inside the VM without rebooting it.
func (s *Server) handleBounce(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	client, code, err := s.guestAgent(name)
	if err != nil {
		httpError(w, err, code)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := client.Bounce(ctx); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleServiceList reports the VM's supervised services.
func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	client, code, err := s.guestAgent(name)
	if err != nil {
		httpError(w, err, code)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	svcs, err := client.ServiceList(ctx)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	if svcs == nil {
		svcs = []agentclient.ServiceState{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(svcs)
}

// handleServiceAdd declares a service and starts supervising it.
//
// The declaration is also persisted on the VM, so a stop/start replays it. The
// agent's registry is live state that dies with the guest; the spec is the
// statement of what should be running.
func (s *Server) handleServiceAdd(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req ServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, fmt.Errorf("invalid request: %w", err), http.StatusBadRequest)
		return
	}
	if !state.ValidServiceName(req.Name) {
		httpError(w, fmt.Errorf("invalid service name %q", req.Name), http.StatusBadRequest)
		return
	}
	if req.Run == "" {
		httpError(w, fmt.Errorf("service %q has no command", req.Name), http.StatusBadRequest)
		return
	}

	client, code, err := s.guestAgent(name)
	if err != nil {
		httpError(w, err, code)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := client.ServiceAdd(ctx, req.Name, req.Run, req.WorkDir, req.Restart, req.Env); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	if err := s.persistService(name, req, false); err != nil {
		httpError(w, fmt.Errorf("service started but could not be persisted: %w", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleServiceDelete stops a service and drops its declaration.
func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	name, svc := r.PathValue("name"), r.PathValue("service")
	client, code, err := s.guestAgent(name)
	if err != nil {
		httpError(w, err, code)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := client.ServiceRemove(ctx, svc); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	if err := s.persistService(name, ServiceRequest{Name: svc}, true); err != nil {
		httpError(w, fmt.Errorf("service stopped but declaration remains: %w", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleServiceRestart restarts one service now.
func (s *Server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	name, svc := r.PathValue("name"), r.PathValue("service")
	client, code, err := s.guestAgent(name)
	if err != nil {
		httpError(w, err, code)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := client.ServiceRestart(ctx, svc); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleServiceLogs returns a service's retained output.
func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	name, svc := r.PathValue("name"), r.PathValue("service")
	tail := 100
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tail = n
		}
	}
	client, code, err := s.guestAgent(name)
	if err != nil {
		httpError(w, err, code)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	lines, err := client.ServiceLogs(ctx, svc, tail)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	if lines == nil {
		lines = []agentclient.LogLine{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lines)
}

// persistService records or removes a service declaration on the VM.
func (s *Server) persistService(vmName string, req ServiceRequest, remove bool) error {
	return s.store.UpdateVM(vmName, func(v *state.VM) {
		if v.Spec == nil {
			v.Spec = &state.VMSpec{}
		}
		out := v.Spec.Services[:0:0]
		for _, existing := range v.Spec.Services {
			if existing.Name != req.Name {
				out = append(out, existing)
			}
		}
		if !remove {
			out = append(out, state.Service{
				Name: req.Name, Run: req.Run, WorkDir: req.WorkDir,
				Env: req.Env, Restart: req.Restart,
			})
		}
		v.Spec.Services = out
	})
}
