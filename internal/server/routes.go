package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/egressdns"
	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/state"
	vmpkg "github.com/agentstep/mvm/internal/vm"
)

func snapshotsBaseDir() string { return filepath.Join(firecracker.DataDir(), "snapshots") }

// baseImageName is the shared base rootfs (base.ext4) that every VM is cloned
// from and that `mvm build` layers on top of. It is not a user-built image:
// the list, inspect, and delete handlers all exclude it, so it stays out of
// `image ls`/`image prune` and can't be inspected or removed by name. Deleting
// it would require a full re-init to recover.
const baseImageName = "base"

// --- Request/Response types ---

type CreateVMRequest struct {
	Name      string          `json:"name"`
	Cpus      int             `json:"cpus,omitempty"`
	MemoryMB  int             `json:"memory_mb,omitempty"`
	Ports     []state.PortMap `json:"ports,omitempty"`
	NetPolicy string          `json:"net_policy,omitempty"`
	Volumes   []string        `json:"volumes,omitempty"`
	Seccomp   string          `json:"seccomp,omitempty"`
	Image     string          `json:"image,omitempty"`
	// Secrets holds attached secret NAMES ONLY — never values. See the
	// package-level security invariant in this plan's Global Constraints.
	Secrets []string `json:"secrets,omitempty"`
	// IdleTimeout and ArchiveAfter drive the daemon's idle tiering (internal/server/tiering.go).
	// Durations like "5m" / "1h"; empty disables that tier. Without these there is no way to
	// enable tiering at all — the sweep runs but every VM opts out, which is exactly what shipped
	// when the fields existed on state.VM and on no request.
	IdleTimeout  string `json:"idle_timeout,omitempty"`
	ArchiveAfter string `json:"archive_after,omitempty"`
}

// BuildRequest is the body for POST /build.
type BuildRequest struct {
	ImageName string                  `json:"image_name"`
	Steps     []firecracker.BuildStep `json:"steps"`
	SizeMB    int                     `json:"size_mb,omitempty"`
}

// ImageInfo describes a custom rootfs image.
type ImageInfo struct {
	Name   string `json:"name"`
	SizeMB int    `json:"size_mb"`
	Digest string `json:"digest,omitempty"` // "sha256:<hex>", computed on inspect
}

type ExecRequest struct {
	Command     string `json:"command"`
	Stdin       string `json:"stdin,omitempty"`
	Stream      bool   `json:"stream,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
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

// VMStats is a point-in-time resource-usage snapshot for one VM (v1: no
// streaming — see handleStatsVMs). Backend split mirrors VMResponse: the
// daemon only ever reports Firecracker VMs (Error is set, not omitted-and-
// silent, when a running VM's stats couldn't be read, e.g. between "marked
// running" and the process actually being observable).
type VMStats struct {
	Name    string  `json:"name"`
	Backend string  `json:"backend,omitempty"`
	PID     int     `json:"pid,omitempty"`
	CPUPct  float64 `json:"cpu_pct"`
	MemMB   float64 `json:"mem_mb"`
	// CPUUsageUsec is cumulative CPU microseconds (monotonic). Additive,
	// omitempty so existing clients that don't expect it are unaffected.
	CPUUsageUsec uint64 `json:"cpu_usage_usec,omitempty"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

// VMInspectResponse is VMResponse plus the persisted declarative spec.
type VMInspectResponse struct {
	VMResponse
	Spec *state.VMSpec `json:"spec,omitempty"`
}

// InspectResponseFromVM shapes a state.VM into the response inspect returns,
// on either backend. Both the daemon's own GET /vms/{name} handler and the
// CLI's local-store path (for applevz VMs, which the daemon never sees) call
// this so the two converge on one schema and internal runtime fields
// (SocketPath, TAPIP, TAPDevice, GuestMAC, RootfsPath) never leak into output.
func InspectResponseFromVM(vm *state.VM) VMInspectResponse {
	return VMInspectResponse{
		VMResponse: VMResponse{
			Name:      vm.Name,
			Status:    vm.Status,
			GuestIP:   vm.GuestIP,
			PID:       vm.PID,
			Backend:   vm.Backend,
			Ports:     vm.Ports,
			CreatedAt: vm.CreatedAt,
		},
		Spec: vm.Spec,
	}
}

// specFromCreateRequest records the create request as a declarative spec,
// persisted on the VM and returned by inspect.
func specFromCreateRequest(req CreateVMRequest) *state.VMSpec {
	return &state.VMSpec{
		Image:     req.Image,
		Cpus:      req.Cpus,
		MemoryMB:  req.MemoryMB,
		Ports:     req.Ports,
		Volumes:   req.Volumes,
		NetPolicy: req.NetPolicy,
		Seccomp:   req.Seccomp,
		Secrets:   req.Secrets,

		IdleTimeout:  req.IdleTimeout,
		ArchiveAfter: req.ArchiveAfter,
	}
}

// SnapshotCreateRequest is the optional body for POST /vms/{name}/snapshot.
type SnapshotCreateRequest struct {
	Name string `json:"name,omitempty"`
}

// SnapshotRestoreRequest is the body for POST /vms/{name}/restore.
type SnapshotRestoreRequest struct {
	Name string `json:"name"`
}

// SnapshotInfo describes a snapshot for listing.
type SnapshotInfo struct {
	Name    string `json:"name"`
	VM      string `json:"vm,omitempty"`
	Created string `json:"created,omitempty"`
	Type    string `json:"type,omitempty"`
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

// handleStatsVMs reports point-in-time CPU/memory for every Firecracker VM
// the daemon knows about. applevz VMs are never included here — the daemon
// has never heard of them (same split as handleListVMs's CLI-side caller,
// internal/cli/list.go's localApplevzVMs); the CLI's own mvm stats command
// merges those in separately via a direct host-local ps call, since the
// applevz mvm-vz helper's PID lives on the macOS host, not inside Lima.
func (s *Server) handleStatsVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.store.ListVMs()
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	result := make([]VMStats, 0, len(vms))
	for _, vm := range vms {
		if vm.Backend == "applevz" {
			continue
		}
		st := VMStats{Name: vm.Name, Backend: vm.Backend, PID: vm.PID, Status: vm.Status}
		if vm.Status == "running" && vm.PID > 0 {
			// One /proc read per VM covers cumulative CPU, memory, and CPU%.
			// This used to be two `ps` spawns per VM per poll (four processes,
			// counting the `bash -c` each goes through) on a 2s dashboard tick.
			//
			// The error is surfaced, not swallowed: CPUUsageUsec is
			// `omitempty`, so a silently-dropped failure is byte-identical to
			// a genuine zero — exactly the symptom this field was added to
			// fix, and undebuggable without a signal.
			sample, err := firecracker.ProcessCumulativeStats(s.executor, vm.PID)
			if err != nil {
				st.Error = err.Error()
				log.Printf("VM %s: stats read failed: %v", vm.Name, err)
			} else {
				st.CPUPct, st.MemMB, st.CPUUsageUsec = sample.CPUPct, sample.MemMB, sample.CPUUsec
			}
		}
		result = append(result, st)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// postBootSetup runs the guest-agent-dependent setup shared by create and
// start-resume: wait for the agent, configure guest networking, then apply
// port forwarding, network policy, volume copy-in, and seccomp from the
// persisted spec. Returns the volume copy-in error if any; network/policy/
// seccomp failures are logged but non-fatal (matching the prior inline behavior).
func (s *Server) postBootSetup(name string, alloc state.NetAllocation, volumes []string, seccomp string, policy state.ParsedNetPolicy) error {
	if !firecracker.WaitForGuest(s.executor, alloc.GuestIP, 120*time.Second) {
		log.Printf("VM %s: guest agent not reachable after 120s", name)
		return fmt.Errorf("guest agent not reachable after 120s")
	}
	// Under an allow: policy the guest must resolve through the egress proxy on
	// its own gateway; the filter drops DNS to anywhere else.
	resolverIP := "8.8.8.8"
	if policy.Mode == state.NetPolicyAllow {
		resolverIP = alloc.TAPIP
	}
	firecracker.SetupGuestNetworkViaAgent(s.executor, alloc.GuestIP, alloc.TAPIP, resolverIP)

	postVM, err := s.store.GetVM(name)
	if err != nil {
		log.Printf("VM %s: failed to reload state for post-boot setup: %v", name, err)
		return err
	}
	if err := firecracker.SetupPortForwarding(s.executor, postVM); err != nil {
		log.Printf("VM %s: port forwarding setup failed: %v", name, err)
	}
	var volErr error
	if len(volumes) > 0 {
		if err := firecracker.SetupVolumeMounts(postVM, volumes); err != nil {
			log.Printf("VM %s: volume mount setup failed: %v", name, err)
			volErr = err
		}
	}
	if seccomp != "" {
		if err := firecracker.ApplySeccompViaAgent(s.executor, postVM, seccomp); err != nil {
			log.Printf("VM %s: seccomp setup failed: %v", name, err)
		}
	}
	return volErr
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

	// applevz VMs are created and booted entirely separately from the
	// Firecracker path below. This daemon previously had NO applevz create
	// path at all — a bare POST here always created a Firecracker VM
	// regardless of the locally configured backend, and the resulting VM's
	// state record never even got a Backend field, so handleExec's hardcoded
	// FirecrackerVsockDialer always 500'd against it. See
	// vm.AppleVZBackend.CreateAndBoot's doc comment for exactly what this
	// minimal path does and doesn't support.
	// Validate the tiering thresholds HERE, before the backend fork. parseThreshold
	// treats anything unparseable as "no threshold", so without this a typo like
	// idle_timeout="5min" would 201 and then never tier — the caller has a success
	// response for cost control that was silently discarded.
	for _, f := range []struct{ name, val string }{
		{"idle_timeout", req.IdleTimeout},
		{"archive_after", req.ArchiveAfter},
	} {
		if f.val == "" {
			continue
		}
		if _, ok := parseThreshold(f.val); !ok {
			httpError(w, fmt.Errorf("invalid %s %q: want a positive Go duration such as \"5m\" or \"1h\"", f.name, f.val), http.StatusBadRequest)
			return
		}
	}
	// Same reasoning as the net_policy refusal further down: the tiering sweep only
	// walks Firecracker VMs, so accepting a threshold for an applevz VM would be a
	// billing promise this daemon does not keep.
	if s.store.GetBackend() == "applevz" {
		if req.IdleTimeout != "" || req.ArchiveAfter != "" {
			httpError(w, fmt.Errorf(
				"idle_timeout/archive_after are not yet honoured for applevz VMs; "+
					"the tiering sweep only walks firecracker VMs, so refusing rather than "+
					"accepting a threshold that would never fire",
			), http.StatusBadRequest)
			return
		}
		s.handleCreateApplevzVM(w, req)
		return
	}

	// Validate ports before any of their fields reach SetupPortForwarding,
	// which interpolates HostIP/Proto into a root `sudo iptables` shell
	// command. A remote client can POST an arbitrary body, so this server-side
	// check — not just the CLI's parsePorts — is the load-bearing one.
	for _, p := range req.Ports {
		if err := state.ValidatePort(p); err != nil {
			httpError(w, err, http.StatusBadRequest)
			return
		}
	}

	// A non-empty Image is interpolated into a root bash script (config.go's
	// StartScriptWithImage, IMAGE_PATH=...) and a CacheDir path, so restrict it
	// to safe characters exactly like handleBuild does — otherwise a crafted
	// image name is a path-traversal/injection vector on the create path. Empty
	// means the default base image and is left alone.
	if req.Image != "" {
		if err := state.ValidateName(req.Image); err != nil || req.Image == "." || req.Image == ".." {
			httpError(w, fmt.Errorf("invalid image %q", req.Image), http.StatusBadRequest)
			return
		}
	}

	now := time.Now()
	vm := &state.VM{
		Name:      req.Name,
		Status:    "starting",
		Ports:     req.Ports,
		NetPolicy: req.NetPolicy,
		Cpus:      req.Cpus,
		MemoryMB:  req.MemoryMB,
		Secrets:   req.Secrets,
		CreatedAt: now,
		Spec:      specFromCreateRequest(req),

		// These two are what tierOnce reads. Without them on the record the sweep
		// runs every interval and every VM opts out — which is precisely the state
		// this feature shipped in.
		IdleTimeout:  req.IdleTimeout,
		ArchiveAfter: req.ArchiveAfter,
	}
	netIndex, err := s.store.ReserveVM(vm)
	if err != nil {
		httpError(w, err, http.StatusConflict)
		return
	}
	alloc := state.AllocateNet(netIndex)

	// Install the host-side egress filter BEFORE the microVM boots. The guest
	// runs as root and can flush any in-guest rule, so enforcement lives here in
	// Lima; doing it pre-boot also closes the window in which a booting guest
	// could reach the network before its policy landed.
	policy, err := state.ParseNetPolicy(req.NetPolicy)
	if err != nil {
		s.store.RemoveVM(req.Name)
		httpError(w, err, http.StatusBadRequest)
		return
	}
	if err := firecracker.InstallEgressPolicy(s.executor, alloc, policy); err != nil {
		s.store.RemoveVM(req.Name)
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	if policy.Mode == state.NetPolicyAllow {
		sets := egressdns.SetPair{
			V4: firecracker.EgressIPSetName(alloc.Index),
			V6: firecracker.EgressIPSetName6(alloc.Index),
		}
		if err := s.dns.Start(alloc, sets, policy.Domains); err != nil {
			firecracker.RemoveEgressPolicy(s.executor, vm)
			s.dns.Stop(vm.NetIndex)
			s.store.RemoveVM(req.Name)
			httpError(w, err, http.StatusInternalServerError)
			return
		}
	}

	// If a custom image is specified, verify it exists.
	if req.Image != "" {
		imagePath := firecracker.CacheDir() + "/" + req.Image + ".ext4"
		if _, err := os.Stat(imagePath); err != nil {
			s.store.RemoveVM(req.Name)
			httpError(w, fmt.Errorf("image %q not found (expected %s)", req.Image, imagePath), http.StatusBadRequest)
			return
		}
	}

	var pid int
	var socketPath string

	// Custom images can't use pooled VMs — the pool uses base.ext4.
	usePool := req.Image == "" &&
		(req.Cpus <= 0 || req.Cpus == firecracker.GuestVcpuCount) &&
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
		if req.Image != "" {
			pid, err = firecracker.StartWithImage(s.executor, req.Name, alloc, req.Cpus, req.MemoryMB, req.Image)
		} else {
			pid, err = firecracker.Start(s.executor, req.Name, alloc, req.Cpus, req.MemoryMB)
		}
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

	// postBoot runs the setup that can only happen once the guest agent is up.
	// It returns the volume copy-in error (if any); network/policy/seccomp
	// failures are logged but non-fatal, matching prior behavior.
	postBoot := func() error {
		return s.postBootSetup(req.Name, alloc, req.Volumes, req.Seccomp, policy)
	}

	// When volumes are requested, the client execs into the guest the moment
	// this create returns — so the tar copy-in must be finished by then, or the
	// command races an empty mount (a silent, timing-dependent wrong result).
	// Run post-boot setup synchronously for the -V case and fail the create if
	// the copy-in fails, tearing the half-created VM down so the caller sees a
	// real error instead of an empty directory. Volume-free VMs keep the fast
	// fire-and-forget path.
	if len(req.Volumes) > 0 {
		if err := postBoot(); err != nil {
			if cvm, gerr := s.store.GetVM(req.Name); gerr == nil {
				firecracker.Cleanup(s.executor, cvm)
			}
			s.store.RemoveVM(req.Name)
			httpError(w, fmt.Errorf("volume setup: %w", err), http.StatusInternalServerError)
			return
		}
	} else {
		go func() { _ = postBoot() }()
	}

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

// handleCreateApplevzVM is handleCreateVM's applevz counterpart — split out
// because the two backends share almost nothing past this point (no pool,
// no Firecracker socket, no iptables-based network policy, a completely
// different agent transport). See vm.AppleVZBackend.CreateAndBoot's doc
// comment for exactly what this minimal path does and doesn't support;
// anything in that "doesn't" list 400s here rather than silently ignoring it.
func (s *Server) handleCreateApplevzVM(w http.ResponseWriter, req CreateVMRequest) {
	if len(req.Ports) > 0 || len(req.Volumes) > 0 || req.Seccomp != "" || req.Image != "" || len(req.Secrets) > 0 {
		httpError(w, fmt.Errorf(
			"ports/volumes/seccomp/image/secrets are not yet supported for applevz VMs created via the daemon API — use the mvm CLI locally (mvm start) for those",
		), http.StatusBadRequest)
		return
	}
	// net_policy gets its own check because getting this wrong is worse than
	// the others. CreateAndBoot applies no network policy, so accepting
	// "deny" here would return 201 Created for a VM with unrestricted network
	// access — the caller believes it asked for a closed sandbox and has a
	// success response saying it got one. Every other unsupported option
	// already 400s rather than being silently dropped; this is the one where
	// silence is a security claim.
	if policy, perr := state.ParseNetPolicy(req.NetPolicy); perr != nil || policy.Mode != state.NetPolicyOpen {
		httpError(w, fmt.Errorf(
			"net_policy is not yet enforced for applevz VMs created via the daemon API; "+
				"refusing rather than returning success for a policy that would not be applied "+
				"(use the mvm CLI locally, or --backend firecracker where the policy is enforced host-side)",
		), http.StatusBadRequest)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		httpError(w, fmt.Errorf("resolve home directory: %w", err), http.StatusInternalServerError)
		return
	}
	backend := vmpkg.NewAppleVZBackend(filepath.Join(home, ".mvm"))

	created, err := backend.CreateAndBoot(s.store, req.Name, req.Cpus, req.MemoryMB)
	if err != nil {
		// CreateAndBoot already cleans up its own partial state (ReserveVM/
		// RemoveVM pairing) on every failure path — nothing further to tear
		// down here.
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMResponse{
		Name:      created.Name,
		Status:    created.Status,
		Backend:   created.Backend,
		PID:       created.PID,
		CreatedAt: created.CreatedAt,
	})
}

// handleStartVM boots an existing STOPPED Firecracker VM in place (cold reboot,
// disk preserved). Additive endpoint for the start-resume verb — never alters
// create. Reuses the VM's existing NetIndex (no ReserveVM), boots the existing
// rootfs via StartExisting, then re-runs the shared post-boot setup.
func (s *Server) handleStartVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "stopped" {
		httpError(w, fmt.Errorf("VM %q is %s, not stopped", name, vm.Status), http.StatusConflict)
		return
	}
	alloc := state.AllocateNet(vm.NetIndex)
	pid, err := firecracker.StartExisting(s.executor, name, alloc, vm.Cpus, vm.MemoryMB)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = alloc.TAPDev
		v.GuestMAC = alloc.GuestMAC
		v.SocketPath = firecracker.SocketPath(name)
		v.PID = pid
		v.RootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"
		v.StoppedAt = nil
	})
	started, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	var volumes []string
	var seccomp string
	if started.Spec != nil {
		volumes = started.Spec.Volumes
		seccomp = started.Spec.Seccomp
	}
	// Re-derive the policy from persisted state and fail closed: a stored
	// policy string that no longer parses must not silently downgrade to open.
	resumePolicy, perr := state.ParseNetPolicy(started.NetPolicy)
	if perr != nil {
		log.Printf("VM %s: unparseable stored policy %q, failing closed to deny: %v", name, started.NetPolicy, perr)
		resumePolicy = state.ParsedNetPolicy{Mode: state.NetPolicyDeny}
	}
	if err := firecracker.InstallEgressPolicy(s.executor, alloc, resumePolicy); err != nil {
		httpError(w, fmt.Errorf("egress policy: %w", err), http.StatusInternalServerError)
		return
	}
	if resumePolicy.Mode == state.NetPolicyAllow {
		sets := egressdns.SetPair{
			V4: firecracker.EgressIPSetName(alloc.Index),
			V6: firecracker.EgressIPSetName6(alloc.Index),
		}
		if err := s.dns.Start(alloc, sets, resumePolicy.Domains); err != nil {
			httpError(w, fmt.Errorf("egress DNS: %w", err), http.StatusInternalServerError)
			return
		}
	}
	postBoot := func() error { return s.postBootSetup(name, alloc, volumes, seccomp, resumePolicy) }
	if len(volumes) > 0 {
		if err := postBoot(); err != nil {
			httpError(w, fmt.Errorf("volume setup: %w", err), http.StatusInternalServerError)
			return
		}
	} else {
		go func() { _ = postBoot() }()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(VMResponse{
		Name:      name,
		Status:    "running",
		GuestIP:   alloc.GuestIP,
		PID:       pid,
		Backend:   started.Backend,
		Ports:     started.Ports,
		CreatedAt: started.CreatedAt,
	})
}

// errNotRunning distinguishes "the VM is in a status exec cannot use" (409) from a genuine failure
// while waking it (500). Both surface from the same locked section, so the caller needs the type to
// pick a status code.
type errNotRunning struct {
	name   string
	status string
}

func (e errNotRunning) Error() string { return fmt.Sprintf("VM %q is %s", e.name, e.status) }

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

	// applevz VMs never reach the Firecracker vsock dial below — that dialer
	// unconditionally assumed every VM was Firecracker-backed (the bug this
	// branch fixes: a VM booted via handleCreateApplevzVM used to 500 on its
	// very first exec, "no such file or directory" on a vsock UDS path that
	// backend never creates). No pause/resume here yet — CreateAndBoot never
	// produces a paused VM — and no interactive/streaming exec; both are
	// explicit gaps (see vm.AppleVZBackend.CreateAndBoot's doc comment), not
	// silently degraded behavior.
	if vm.Backend == "applevz" {
		if vm.Status != "running" {
			httpError(w, fmt.Errorf("VM %q is %s", name, vm.Status), http.StatusConflict)
			return
		}
		if req.Interactive {
			httpError(w, fmt.Errorf("interactive exec is not yet supported for applevz VMs via the daemon API"), http.StatusBadRequest)
			return
		}
		now := time.Now()
		s.store.UpdateVM(name, func(v *state.VM) { v.LastActivity = &now })
		// applevz is not swept today, but the completion stamp is what makes idle age mean idle
		// age rather than command runtime — worth having before the sweep learns this backend.
		defer s.ops.beginExec(name, s.store)()

		home, herr := os.UserHomeDir()
		if herr != nil {
			httpError(w, fmt.Errorf("resolve home directory: %w", herr), http.StatusInternalServerError)
			return
		}
		client := vmpkg.NewAppleVZBackend(filepath.Join(home, ".mvm")).AgentClient(name)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		s.execAndRespond(w, client, req, ctx)
		return
	}

	// Wake the VM and mark the exec in flight as ONE atomic step, under the VM's transition lock.
	//
	// Without the lock the tiering sweep can pause or archive the VM in the window between waking
	// it and running the command — the caller then gets a failure from a VM the daemon itself just
	// froze. Marking in-flight inside the same critical section closes the window completely: the
	// sweep re-checks busy while holding this lock, so it either sees the marker or has already
	// finished its own transition before this one begins.
	//
	// The lock is released before the command runs. A five-minute exec must not block the sweep
	// from touching other VMs, and the in-flight marker — not the lock — is what keeps THIS VM
	// safe for the duration.
	if lerr := s.ops.with(name, func() error {
		// Re-read inside the lock: the status read before it may predate a sweep transition.
		current, gerr := s.store.GetVM(name)
		if gerr != nil {
			return gerr
		}
		switch current.Status {
		case "paused":
			// Auto-resume paused VMs so idle-pause doesn't break exec.
			if err := firecracker.Resume(s.executor, current); err != nil {
				return fmt.Errorf("auto-resume failed: %w", err)
			}
			s.store.UpdateVM(name, func(v *state.VM) { v.Status = "running" })
		case "archived":
			// Same idea one tier down: an archived VM has been checkpointed to disk and its memory
			// released, so restoring is slower than a resume but equally invisible to the caller.
			// If this were a 409 instead, tiering would stop being a memory optimisation and
			// become a contract change every client had to learn.
			if err := s.restoreArchivedVM(current); err != nil {
				return fmt.Errorf("auto-restore failed: %w", err)
			}
		case "running":
		default:
			return errNotRunning{name: name, status: current.Status}
		}
		return nil
	}); lerr != nil {
		var nr errNotRunning
		if errors.As(lerr, &nr) {
			httpError(w, lerr, http.StatusConflict)
		} else {
			httpError(w, lerr, http.StatusInternalServerError)
		}
		return
	}

	// Held for the whole command. done() clears it and stamps LastActivity at COMPLETION —
	// stamping only at the start makes a long command look idle for its entire duration.
	done := s.ops.beginExec(name, s.store)
	defer done()

	// The record may have been replaced by a restore; re-read it so the exec below uses the new
	// network allocation and PID rather than the stale ones.
	vm, err = s.store.GetVM(name)
	if err != nil {
		httpError(w, fmt.Errorf("VM %q vanished after wake: %w", name, err), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	s.store.UpdateVM(name, func(v *state.VM) { v.LastActivity = &now })

	if req.Interactive {
		s.handleInteractiveExec(w, r, name, req.Command)
		return
	}

	// Talk to the guest agent over Firecracker's vsock UDS bridge.
	client := agentclient.New(&agentclient.FirecrackerVsockDialer{
		UDSPath: firecracker.VsockUDSPath(name),
	})

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	s.execAndRespond(w, client, req, ctx)
}

// execAndRespond runs one exec through client (Firecracker- or
// applevz-backed — agentclient.Client doesn't care which dialer built it)
// and writes the response in whichever shape req asked for. Shared by both
// backends in handleExec so the NDJSON-vs-JSON framing logic exists exactly
// once.
func (s *Server) execAndRespond(w http.ResponseWriter, client *agentclient.Client, req ExecRequest, ctx context.Context) {
	if req.Stream {
		s.execStreamAndRespond(w, client, req, ctx)
		return
	}

	result, execErr := client.Exec(ctx, req.Command, req.Stdin)
	if execErr != nil {
		httpError(w, execErr, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ExecResponse{Output: result.Output, ExitCode: result.ExitCode})
}

// streamWriteTimeout bounds a single frame write, not the whole command. The listener's
// WriteTimeout cannot serve here: it is armed once, before the handler runs.
const streamWriteTimeout = 30 * time.Second

// execStreamAndRespond streams NDJSON frames as the command produces them.
//
// stream:true used to be a lie: the handler called the blocking Exec, waited for the command to
// finish, and then emitted the whole buffer as a single stdout frame followed by an exit frame.
// The wire shape was right, so clients could not tell — but nothing arrived until the command was
// over, which makes watching a build impossible and is the reason the TS provider's spawn() hands
// back a "stream" that is really a finished string.
//
// The frame vocabulary is unchanged, so a client that reads NDJSON until the exit frame keeps
// working: the old behaviour was a degenerate case of this one, with every byte in a single frame.
// stderr now arrives as its own frame type — the guest has always sent it separately and the split
// was being discarded here.
func (s *Server) execStreamAndRespond(w http.ResponseWriter, client *agentclient.Client, req ExecRequest, ctx context.Context) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)

	// The TCP listener's 5-minute WriteTimeout was armed before this handler ran, and a streaming
	// response outlives it — the write simply starts failing mid-command. Push the deadline out on
	// every frame instead, so the bound is per-write rather than per-command.
	writeFrame := func(v any) error {
		if derr := rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); derr != nil && !errors.Is(derr, http.ErrNotSupported) {
			return derr
		}
		line, merr := json.Marshal(v)
		if merr != nil {
			return merr
		}
		if _, werr := w.Write(append(line, '\n')); werr != nil {
			return werr
		}
		return rc.Flush()
	}

	exitCode, err := client.ExecStream(ctx, req.Command, req.Stdin, func(f agentclient.ExecStreamFrame) error {
		switch f.Kind {
		case "exit":
			return writeFrame(map[string]any{"type": "exit", "exit_code": f.ExitCode})
		default:
			return writeFrame(map[string]any{"type": f.Kind, "data": string(f.Data)})
		}
	})
	if err != nil {
		// The header is already sent, so this cannot become a status code. An error frame is the
		// only way to tell the client the difference between "the command ended" and "we stopped
		// being able to watch it" — silence would read as a clean exit.
		log.Printf("exec stream: %v", err)
		_ = writeFrame(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	_ = exitCode
}

// handleInteractiveExec hijacks the HTTP connection and bridges it to the
// guest agent's exec_pty endpoint. After the initial handshake, the daemon
// is a transparent bidirectional relay — length-prefixed JSON frames pass
// through unmodified between the CLI client and the guest agent.
func (s *Server) handleInteractiveExec(w http.ResponseWriter, r *http.Request, vmName, command string) {
	// 1. Dial the guest agent via vsock.
	dialer := &agentclient.FirecrackerVsockDialer{
		UDSPath: firecracker.VsockUDSPath(vmName),
	}
	agentConn, err := dialer.Dial(r.Context())
	if err != nil {
		httpError(w, fmt.Errorf("dial agent: %w", err), http.StatusInternalServerError)
		return
	}
	defer agentConn.Close()

	// 2. Send exec_pty request to the agent — must match the agent's
	// protocol.Request wire format: type + id at top level, pty nested.
	agentReq := struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Pty  struct {
			Command string `json:"command"`
			Rows    int    `json:"rows"`
			Cols    int    `json:"cols"`
			Term    string `json:"term,omitempty"`
		} `json:"pty"`
	}{
		Type: "exec_pty",
		ID:   agentclient.NewID(),
	}
	agentReq.Pty.Command = command
	agentReq.Pty.Rows = 24
	agentReq.Pty.Cols = 80
	agentReq.Pty.Term = "xterm-256color"
	if err := agentclient.WriteFrame(agentConn, agentReq); err != nil {
		httpError(w, fmt.Errorf("send exec_pty: %w", err), http.StatusInternalServerError)
		return
	}

	// 3. Read the agent's initial OK response.
	var agentResp agentclient.ExecPtyResponse
	if err := agentclient.ReadFrame(agentConn, &agentResp); err != nil {
		httpError(w, fmt.Errorf("read agent response: %w", err), http.StatusInternalServerError)
		return
	}
	if agentResp.Type != "ok" {
		httpError(w, fmt.Errorf("agent error: %s", agentResp.Error), http.StatusInternalServerError)
		return
	}

	// 4. Hijack the HTTP connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		httpError(w, fmt.Errorf("server does not support hijacking"), http.StatusInternalServerError)
		agentConn.Close()
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		httpError(w, fmt.Errorf("hijack: %w", err), http.StatusInternalServerError)
		agentConn.Close()
		return
	}
	defer conn.Close()

	// Clear the deadlines net/http armed before this handler ran.
	//
	// The TCP listener sets ReadTimeout 30s and WriteTimeout 5m (server.go). Those are applied to
	// the connection up front, and Hijack does NOT clear them — so over TCP, an interactive session
	// starts failing reads about 30 seconds in, and any long-lived relay dies at five minutes. The
	// Unix-socket listener sets no timeouts, which is why this has never been seen locally: it is a
	// remote-only failure, on the one transport a containerised client must use.
	//
	// A hijacked connection is no longer request/response shaped, so a request-scoped deadline is
	// meaningless on it; the session ends when either side closes.
	if derr := conn.SetDeadline(time.Time{}); derr != nil {
		log.Printf("interactive exec %s: could not clear connection deadline: %v", vmName, derr)
	}

	// 5. Write HTTP 101 Switching Protocols response.
	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Upgrade: tty\r\n")
	bufrw.WriteString("\r\n")
	bufrw.Flush()

	// 6. Bidirectional relay: transparent bridge between client and agent.
	done := make(chan struct{}, 2)

	// Agent -> Client
	go func() {
		io.Copy(conn, agentConn)
		done <- struct{}{}
	}()

	// Client -> Agent
	go func() {
		io.Copy(agentConn, conn)
		done <- struct{}{}
	}()

	// Wait for either direction to finish, then clean up both.
	<-done
	conn.Close()
	agentConn.Close()
	<-done

	log.Printf("VM %s: interactive exec session ended", vmName)
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	if vm.Backend == "applevz" {
		// Parity with firecracker.Cleanup below: stop the process (graceful
		// IPC stop, SIGTERM fallback — AppleVZBackend.StopVM already does
		// both) and remove the per-VM directory. Best-effort, same as
		// Cleanup's own error handling (nothing here blocks the state removal).
		if home, herr := os.UserHomeDir(); herr == nil {
			backend := vmpkg.NewAppleVZBackend(filepath.Join(home, ".mvm"))
			_ = backend.StopVM(name, vm.PID)
			_ = os.RemoveAll(filepath.Join(home, ".mvm", "vms", name))
		}
		s.store.RemoveVM(name)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	firecracker.Cleanup(s.executor, vm)
	s.store.RemoveVM(name)

	w.WriteHeader(http.StatusNoContent)
}

type StopVMRequest struct {
	Force bool `json:"force,omitempty"`
}

func (s *Server) handleStopVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req StopVMRequest
	// Body is optional — default to graceful stop.
	json.NewDecoder(r.Body).Decode(&req)

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "running" && vm.Status != "paused" {
		httpError(w, fmt.Errorf("VM %q is %s", name, vm.Status), http.StatusConflict)
		return
	}

	// Remove port forwarding before stopping.
	firecracker.RemovePortForwarding(s.executor, vm)
	firecracker.RemoveEgressPolicy(s.executor, vm)
	s.dns.Stop(vm.NetIndex)

	if req.Force {
		// Force kill — no graceful shutdown attempt.
		s.executor.Run(fmt.Sprintf("sudo kill -9 %d 2>/dev/null || true", vm.PID))
		s.executor.Run(fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true",
			firecracker.SocketPath(name), vm.TAPDevice))
	} else {
		// Resume paused VMs before graceful shutdown (needed for agent
		// to process the poweroff command).
		if vm.Status == "paused" {
			firecracker.Resume(s.executor, vm)
		}
		hostKeyPath := firecracker.KeyDir() + "/mvm.id_ed25519"
		firecracker.StopViaAgent(s.executor, vm, hostKeyPath)
	}

	now := time.Now()
	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "stopped"
		v.StoppedAt = &now
	})

	w.WriteHeader(http.StatusNoContent)
}

// handleArchiveVM checkpoints a VM to disk and then stops it, releasing its RAM.
//
// This is the operation pause is not. Pause freezes vCPUs and leaves memory untouched, so a paused
// VM still occupies the resource that actually limits how many sandboxes a host holds. Snapshot on
// its own does not help either — it pauses, checkpoints and RESUMES, so the VM keeps running and
// keeps its memory. Archive is snapshot followed by stop: the state survives on disk, the memory
// comes back to the host.
//
// Ordering is load-bearing. The snapshot must be written and verified BEFORE anything kills the
// VM; reversing it turns a full memory image into an unrecoverable sandbox. A failed snapshot
// leaves the VM exactly as it was and returns an error.
//
// Reverse with POST /vms/{name}/restore, which brings the VM back under the same name.
func (s *Server) handleArchiveVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status == "archived" {
		// Idempotent: archiving an archived VM is what a retrying caller does, and re-snapshotting
		// a stopped VM would fail confusingly.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if vm.Status != "running" && vm.Status != "paused" {
		httpError(w, fmt.Errorf("VM %q is %s, must be running or paused to archive", name, vm.Status), http.StatusConflict)
		return
	}

	snapName, err := s.archiveVM(vm)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":   "archived",
		"snapshot": snapName,
		"memory":   fmt.Sprintf("%dMB released", vm.MemoryMB),
	})
}

// archiveVM is the operation itself, shared by the HTTP handler and the idle sweep so the two
// cannot drift. Returns the snapshot name it checkpointed to.
//
// Ordering is load-bearing: the snapshot must be on disk before anything kills the VM. Reversed,
// a crash between the two turns a live sandbox into nothing. A failed snapshot returns an error
// with the VM untouched and still running.
func (s *Server) archiveVM(vm *state.VM) (string, error) {
	snapName := vm.Name + "-archive"
	if err := state.ValidateName(snapName); err != nil {
		return "", fmt.Errorf("invalid archive name %q: %w", snapName, err)
	}

	// 1. Checkpoint. Nothing destructive happens until this succeeds.
	snapDir := filepath.Join(snapshotsBaseDir(), snapName)
	if err := firecracker.SnapshotVM(s.executor, vm, snapDir); err != nil {
		return "", fmt.Errorf("archive %q: snapshot failed, VM left running: %w", vm.Name, err)
	}

	// 2. Release the memory. Force-kill rather than a graceful agent shutdown: the guest state is
	// already captured, so running shutdown scripts would diverge from the checkpoint just taken.
	firecracker.RemovePortForwarding(s.executor, vm)
	firecracker.RemoveEgressPolicy(s.executor, vm)
	s.dns.Stop(vm.NetIndex)
	s.executor.Run(fmt.Sprintf("sudo kill -9 %d 2>/dev/null || true", vm.PID))
	s.executor.Run(fmt.Sprintf("sudo rm -f %s; sudo ip link del %s 2>/dev/null || true",
		firecracker.SocketPath(vm.Name), vm.TAPDevice))
	if vm.UFFDPid != 0 {
		firecracker.KillUFFDHandler(vm.UFFDPid)
	}

	now := time.Now()
	s.store.UpdateVM(vm.Name, func(v *state.VM) {
		v.Status = "archived"
		v.ArchivedSnapshot = snapName
		v.StoppedAt = &now
		v.UFFDPid = 0
	})
	return snapName, nil
}

func (s *Server) handlePauseVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "running" {
		httpError(w, fmt.Errorf("VM %q is %s, cannot pause", name, vm.Status), http.StatusConflict)
		return
	}

	if err := firecracker.Pause(s.executor, vm); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	s.store.UpdateVM(name, func(v *state.VM) { v.Status = "paused" })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResumeVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "paused" {
		httpError(w, fmt.Errorf("VM %q is %s, cannot resume", name, vm.Status), http.StatusConflict)
		return
	}

	if err := firecracker.Resume(s.executor, vm); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	s.store.UpdateVM(name, func(v *state.VM) { v.Status = "running" })
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

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}
	if vm.Status != "running" && vm.Status != "paused" {
		httpError(w, fmt.Errorf("VM %q is %s, must be running or paused", name, vm.Status), http.StatusConflict)
		return
	}

	var req SnapshotCreateRequest
	// Body is optional — ignore decode errors.
	json.NewDecoder(r.Body).Decode(&req)
	snapName := req.Name
	if snapName == "" {
		snapName = name + "-snap"
	}
	// Validate: snapName is interpolated into shell commands in SnapshotVM and
	// used as a path component. ValidateName restricts to [a-zA-Z0-9._-], which
	// blocks shell metacharacters and path separators (prevents command
	// injection / path traversal). Also reject ".." explicitly.
	if err := state.ValidateName(snapName); err != nil || snapName == "." || snapName == ".." {
		httpError(w, fmt.Errorf("invalid snapshot name %q", snapName), http.StatusBadRequest)
		return
	}

	snapDir := filepath.Join(snapshotsBaseDir(), snapName)
	if err := firecracker.SnapshotVM(s.executor, vm, snapDir); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"snapshot": snapName,
		"status":   "created",
	})
}

func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req SnapshotRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, fmt.Errorf("invalid request: %w", err), http.StatusBadRequest)
		return
	}
	if err := state.ValidateName(req.Name); err != nil || req.Name == "." || req.Name == ".." {
		httpError(w, fmt.Errorf("invalid snapshot name %q", req.Name), http.StatusBadRequest)
		return
	}

	snapDir := filepath.Join(snapshotsBaseDir(), req.Name)
	if _, err := os.Stat(filepath.Join(snapDir, "meta.json")); err != nil {
		httpError(w, fmt.Errorf("snapshot %q not found", req.Name), http.StatusNotFound)
		return
	}

	// If VM exists and is running or paused, stop it first.
	vm, err := s.store.GetVM(name)
	if err == nil && (vm.Status == "running" || vm.Status == "paused") {
		firecracker.RemovePortForwarding(s.executor, vm)
		firecracker.RemoveEgressPolicy(s.executor, vm)
		s.dns.Stop(vm.NetIndex)
		if vm.Status == "paused" {
			firecracker.Resume(s.executor, vm)
		}
		hostKeyPath := firecracker.KeyDir() + "/mvm.id_ed25519"
		firecracker.StopViaAgent(s.executor, vm, hostKeyPath)
		now := time.Now()
		s.store.UpdateVM(name, func(v *state.VM) {
			v.Status = "stopped"
			v.StoppedAt = &now
		})
	}

	// Remove existing VM entry if present so we can reserve a fresh one.
	if err == nil {
		s.store.RemoveVM(name)
	}

	// Reserve a new VM entry with a network allocation.
	newVM := &state.VM{
		Name:      name,
		Status:    "restoring",
		CreatedAt: time.Now(),
	}
	netIndex, err := s.store.ReserveVM(newVM)
	if err != nil {
		httpError(w, err, http.StatusConflict)
		return
	}
	alloc := state.AllocateNet(netIndex)

	pid, socketPath, uffdPid, err := firecracker.RestoreVMSnapshot(s.executor, name, snapDir, alloc)
	if err != nil {
		s.store.RemoveVM(name)
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	s.store.UpdateVM(name, func(v *state.VM) {
		v.Status = "running"
		v.GuestIP = alloc.GuestIP
		v.TAPIP = alloc.TAPIP
		v.TAPDevice = alloc.TAPDev
		v.GuestMAC = alloc.GuestMAC
		v.SocketPath = socketPath
		v.PID = pid
		v.UFFDPid = uffdPid
		v.RootfsPath = firecracker.VMDir(name) + "/rootfs.ext4"
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"snapshot": req.Name,
		"status":   "restored",
	})
}

func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	names, err := firecracker.ListSnapshots(snapshotsBaseDir())
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	result := make([]SnapshotInfo, 0, len(names))
	for _, n := range names {
		info := SnapshotInfo{Name: n}
		metaPath := filepath.Join(snapshotsBaseDir(), n, "meta.json")
		data, err := os.ReadFile(metaPath)
		if err == nil {
			var meta map[string]string
			if json.Unmarshal(data, &meta) == nil {
				info.VM = meta["vm"]
				info.Created = meta["created"]
				info.Type = meta["type"]
			}
		}
		result = append(result, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	snapDir := filepath.Join(snapshotsBaseDir(), name)
	if _, err := os.Stat(snapDir); err != nil {
		httpError(w, fmt.Errorf("snapshot %q not found", name), http.StatusNotFound)
		return
	}

	if err := os.RemoveAll(snapDir); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	var req BuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, fmt.Errorf("invalid request: %w", err), http.StatusBadRequest)
		return
	}

	// ImageName is interpolated into shell paths and a generated build script
	// (firecracker.BuildRootfs), so restrict it to safe characters to prevent
	// command injection.
	if err := state.ValidateName(req.ImageName); err != nil || req.ImageName == "." || req.ImageName == ".." {
		httpError(w, fmt.Errorf("invalid image_name %q", req.ImageName), http.StatusBadRequest)
		return
	}
	if len(req.Steps) == 0 {
		httpError(w, fmt.Errorf("steps must not be empty"), http.StatusBadRequest)
		return
	}

	sizeMB := req.SizeMB
	if sizeMB <= 0 {
		sizeMB = 512
	}

	if err := firecracker.BuildRootfs(s.executor, firecracker.CacheDir(), req.ImageName, req.Steps, sizeMB); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"image":  req.ImageName,
		"status": "built",
	})
}

func (s *Server) handleImageList(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(firecracker.CacheDir())
	if err != nil {
		// If the directory doesn't exist, return empty list.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ImageInfo{})
		return
	}

	var images []ImageInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".ext4") {
			continue
		}
		baseName := strings.TrimSuffix(name, ".ext4")
		if baseName == baseImageName {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		sizeMB := int(info.Size() / (1024 * 1024))
		images = append(images, ImageInfo{Name: baseName, SizeMB: sizeMB})
	}

	if images == nil {
		images = []ImageInfo{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(images)
}

// handleImageDownload streams a built custom-image file to the caller. This
// is how a custom image built via `mvm build` — which always runs on the
// daemon's own Linux host (firecracker.CacheDir(), never shared with macOS,
// see the Phase 2 finding in the backend-parity plan) — reaches an applevz
// host, which runs directly on macOS with no daemon and no shared
// filesystem with that Linux host at all.
func (s *Server) handleImageDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// {name} arrives percent-decoded, so it can contain "/" (e.g.
	// "..%2f..%2fetc%2fpasswd"). ValidateName rejects any path separator,
	// which is what stops this from escaping CacheDir into an arbitrary
	// *.ext4 read elsewhere on the host.
	if err := state.ValidateName(name); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	imagePath := filepath.Join(firecracker.CacheDir(), name+".ext4")

	f, err := os.Open(imagePath)
	if err != nil {
		httpError(w, fmt.Errorf("image %q not found (expected %s)", name, imagePath), http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	io.Copy(w, f)
}

func (s *Server) handleImageDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Same traversal guard as handleImageDownload — without it, a crafted
	// {name} could delete an arbitrary *.ext4 file anywhere on the host.
	if err := state.ValidateName(name); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	// Refuse the shared base rootfs. It is hidden from `image ls`, so deleting
	// it by name would silently destroy the image every VM clones from and
	// force a full re-init to recover.
	if name == baseImageName {
		httpError(w, fmt.Errorf("image %q not found", name), http.StatusNotFound)
		return
	}

	imagePath := filepath.Join(firecracker.CacheDir(), name+".ext4")
	if _, err := os.Stat(imagePath); err != nil {
		httpError(w, fmt.Errorf("image %q not found", name), http.StatusNotFound)
		return
	}

	if err := os.Remove(imagePath); err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleImageInspect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := state.ValidateName(name); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	// base.ext4 is the shared base rootfs, not a user-built image.
	// handleImageList hides it and handleImageDelete refuses it, so inspect
	// must too — otherwise `base` is inspectable but unlistable and
	// un-prunable. It is also the largest blob in the cache, i.e. the worst
	// case for the hashing below.
	if name == baseImageName {
		httpError(w, fmt.Errorf("image %q not found", name), http.StatusNotFound)
		return
	}
	path := filepath.Join(firecracker.CacheDir(), name+".ext4")
	// Open once and Stat the handle: os.Stat followed by os.Open is two
	// syscalls with a TOCTOU window between them, and matches neither
	// sibling handler (handleImageDownload opens then f.Stat()s).
	f, err := os.Open(path)
	if err != nil {
		httpError(w, fmt.Errorf("image %q not found", name), http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	if fi.IsDir() {
		httpError(w, fmt.Errorf("image %q not found", name), http.StatusNotFound)
		return
	}

	digest, err := s.imageDigest(r.Context(), path, fi)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Client hung up mid-hash; nothing useful to write.
			return
		}
		httpError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ImageInfo{
		Name:   name,
		SizeMB: int(fi.Size() / (1024 * 1024)),
		Digest: digest,
	})
}

// digestKey identifies a cached digest. Size and mtime together are enough to
// notice a rebuilt image: `mvm build` writes a fresh blob rather than mutating
// one in place, so a changed image always changes at least mtime.
type digestKey struct {
	path  string
	size  int64
	mtime int64
}

// imageDigest returns the sha256 of an image blob, computing it at most once
// per (path, size, mtime).
//
// Without the cache every inspect re-hashes the whole blob. Custom rootfs
// images are GiB-scale, so that is tens of seconds of CPU and IO per call —
// long enough for the TCP listener's WriteTimeout to kill the connection
// before the first byte is written, and cheap enough to loop that an
// authenticated client could pin the daemon with it.
//
// The hash honours ctx so a client that disconnects mid-hash stops the work
// instead of leaving the daemon reading to EOF.
func (s *Server) imageDigest(ctx context.Context, path string, fi os.FileInfo) (string, error) {
	key := digestKey{path: path, size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if v, ok := s.digestCache.Load(key); ok {
		return v.(string), nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	s.digestCache.Store(key, digest)
	return digest, nil
}

// ctxReader aborts a long io.Copy when its context is cancelled. io.Copy
// itself has no cancellation, so without this a disconnected client still
// costs a full read of the file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (s *Server) handleInspectVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	vm, err := s.store.GetVM(name)
	if err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(InspectResponseFromVM(vm))
}

// tailLines returns the last n lines of f (already positioned at the
// start), consuming the file. Loads the whole file into memory — boot logs
// are one VM's console output, small enough that this is simpler than a
// seek-from-end scan.
func tailLines(f *os.File, n int) (string, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	text := string(data)
	trailingNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return out, nil
}

// handleVMLogs serves a VM's Firecracker boot/console log — the file
// showBootLog (internal/cli/logs.go) used to read over limaClient.Shell().
// The daemon runs natively on the same Linux host as this file (Lima's
// guest OS locally, or a cloud server remotely — see firecracker.VMDir), so
// it opens it directly; no shell-out needed. Guest journal logs (the
// non-boot path) already go through the existing exec endpoint and are out
// of scope — this endpoint only ever serves ?boot=true.
func (s *Server) handleVMLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if r.URL.Query().Get("boot") != "true" {
		httpError(w, fmt.Errorf("this endpoint only serves boot logs (?boot=true) — guest logs go through exec"), http.StatusBadRequest)
		return
	}

	if _, err := s.store.GetVM(name); err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	tail := 0
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			tail = n
		}
	}
	follow := r.URL.Query().Get("follow") == "true"

	logPath := filepath.Join(firecracker.VMDir(name), "firecracker.log")
	f, err := os.Open(logPath)
	if err != nil {
		httpError(w, fmt.Errorf("open boot log: %w", err), http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	writeFrame := func(data string) bool {
		frame, _ := json.Marshal(map[string]string{"type": "data", "data": data})
		if _, err := w.Write(append(frame, '\n')); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	// writeErrorFrame emits a terminal {"type":"error"} NDJSON frame. Once
	// streaming has begun the HTTP status is already 200, so an error that
	// occurs mid-stream (only possible in follow mode) can't be reported via
	// httpError — the client (Client.StreamLogs) surfaces this frame instead.
	writeErrorFrame := func(msg string) {
		frame, _ := json.Marshal(map[string]string{"type": "error", "error": msg})
		w.Write(append(frame, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	if tail > 0 {
		lines, err := tailLines(f, tail)
		if err != nil {
			httpError(w, err, http.StatusInternalServerError)
			return
		}
		if !writeFrame(lines) {
			return
		}
	} else {
		data, err := io.ReadAll(f)
		if err != nil {
			httpError(w, err, http.StatusInternalServerError)
			return
		}
		if !writeFrame(string(data)) {
			return
		}
	}

	if !follow {
		return
	}

	// Poll for appended bytes until the client disconnects. There's no
	// inotify in the stdlib and this file has a single appending writer
	// (Firecracker's own redirected stdout — config.go's
	// `>"$VM_DIR/firecracker.log"`), so a short poll loop makes the same
	// trade-off `tail -f` itself makes.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			buf := make([]byte, 4096)
			n, err := f.Read(buf)
			if n > 0 {
				if !writeFrame(string(buf[:n])) {
					return
				}
			}
			if err != nil && err != io.EOF {
				writeErrorFrame(fmt.Sprintf("read boot log: %v", err))
				return
			}
		}
	}
}

func httpError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
