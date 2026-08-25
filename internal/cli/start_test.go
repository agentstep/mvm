package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

func TestParsePorts(t *testing.T) {
	tests := []struct {
		input      []string
		wantLen    int
		wantErr    bool
		wantHostIP string
		wantHost   int
		wantGuest  int
		wantProto  string
	}{
		{[]string{"8080:80"}, 1, false, "", 8080, 80, "tcp"},
		{[]string{"3000:3000"}, 1, false, "", 3000, 3000, "tcp"},
		{[]string{"53:53/udp"}, 1, false, "", 53, 53, "udp"},
		{[]string{"8080:80", "3000:3000"}, 2, false, "", 8080, 80, "tcp"},
		{nil, 0, false, "", 0, 0, ""},
		{[]string{}, 0, false, "", 0, 0, ""},
		{[]string{"invalid"}, 0, true, "", 0, 0, ""},
		{[]string{"abc:80"}, 0, true, "", 0, 0, ""},
		{[]string{"8080:abc"}, 0, true, "", 0, 0, ""},
		{[]string{"127.0.0.1:8080:80"}, 1, false, "127.0.0.1", 8080, 80, "tcp"},
		{[]string{"0.0.0.0:8080:80/udp"}, 1, false, "0.0.0.0", 8080, 80, "udp"},
		{[]string{"192.168.1.5:53:53"}, 1, false, "192.168.1.5", 53, 53, "tcp"},
		{[]string{":8080:80"}, 0, true, "", 0, 0, ""},          // empty host-ip
		{[]string{"1:2:3:4"}, 0, true, "", 0, 0, ""},           // too many segments
		{[]string{"127.0.0.1:abc:80"}, 0, true, "", 0, 0, ""},  // bad host port with host-ip present
		{[]string{"$(id):8080:80"}, 0, true, "", 0, 0, ""},     // injection host-ip (would hit sudo iptables)
		{[]string{"not-an-ip:8080:80"}, 0, true, "", 0, 0, ""}, // non-IP host address
		{[]string{"8080:80/tcp;rm"}, 0, true, "", 0, 0, ""},    // injection proto
	}

	for _, tt := range tests {
		ports, err := parsePorts(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parsePorts(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if len(ports) != tt.wantLen {
			t.Errorf("parsePorts(%v) len = %d, want %d", tt.input, len(ports), tt.wantLen)
			continue
		}
		if tt.wantLen > 0 {
			if ports[0].HostIP != tt.wantHostIP {
				t.Errorf("HostIP = %q, want %q", ports[0].HostIP, tt.wantHostIP)
			}
			if ports[0].HostPort != tt.wantHost {
				t.Errorf("HostPort = %d, want %d", ports[0].HostPort, tt.wantHost)
			}
			if ports[0].GuestPort != tt.wantGuest {
				t.Errorf("GuestPort = %d, want %d", ports[0].GuestPort, tt.wantGuest)
			}
			if ports[0].Proto != tt.wantProto {
				t.Errorf("Proto = %q, want %q", ports[0].Proto, tt.wantProto)
			}
		}
	}
}

// === applevzSpec ===

func TestApplevzSpecCapturesRequest(t *testing.T) {
	ports := []state.PortMap{{HostPort: 3000, GuestPort: 3000, Proto: "tcp"}}
	startup := &StartupSpec{Commands: []StartupCommand{{Name: "dev", Run: "make dev"}}}

	spec := applevzSpec(ports, "deny", 4, 2048, []string{"/h:/g"}, startup, []string{"KEY"})

	if spec.Cpus != 4 || spec.MemoryMB != 2048 || spec.NetPolicy != "deny" {
		t.Errorf("spec = %+v, want cpus=4 mem=2048 policy=deny", spec)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].HostPort != 3000 {
		t.Errorf("spec.Ports = %+v, want the request ports", spec.Ports)
	}
	if len(spec.Secrets) != 1 || spec.Secrets[0] != "KEY" {
		t.Errorf("spec.Secrets = %+v, want [KEY]", spec.Secrets)
	}
	var round StartupSpec
	if err := json.Unmarshal(spec.Startup, &round); err != nil {
		t.Fatalf("Startup should be valid JSON: %v", err)
	}
	if len(round.Commands) != 1 || round.Commands[0].Run != "make dev" {
		t.Errorf("Startup round-trip = %+v, want the recipe", round)
	}
}

func TestApplevzSpecNilStartup(t *testing.T) {
	spec := applevzSpec(nil, "open", 0, 0, nil, nil, nil)
	if spec.Startup != nil {
		t.Errorf("Startup = %s, want nil when no recipe given", spec.Startup)
	}
}

// === parseVolumes ===

func TestParseVolumes(t *testing.T) {
	cwd, _ := os.Getwd()

	tests := []struct {
		name    string
		input   []string
		wantErr bool
		check   func(t *testing.T, got []string)
	}{
		{
			name:  "absolute host path passes through",
			input: []string{"/tmp/src:/app"},
			check: func(t *testing.T, got []string) {
				if len(got) != 1 || got[0] != "/tmp/src:/app" {
					t.Errorf("got %v, want [/tmp/src:/app]", got)
				}
			},
		},
		{
			name:  "relative host path resolves against cwd",
			input: []string{"./src:/app"},
			check: func(t *testing.T, got []string) {
				want := filepath.Join(cwd, "src") + ":/app"
				if len(got) != 1 || got[0] != want {
					t.Errorf("got %v, want [%s]", got, want)
				}
			},
		},
		{name: "missing colon", input: []string{"/tmp/src"}, wantErr: true},
		{name: "empty host path", input: []string{":/app"}, wantErr: true},
		{name: "relative guest path rejected", input: []string{"/tmp/src:app"}, wantErr: true},
		{name: "nil input ok", input: nil},
		{name: "empty slice ok", input: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVolumes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseVolumes(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// === virtiofsMounts ===

func TestVirtiofsMounts(t *testing.T) {
	mounts, err := virtiofsMounts([]string{"/host/a:/data", "/host/b:/app/lib"})
	if err != nil {
		t.Fatalf("virtiofsMounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(mounts))
	}
	// Tag order (vol0, vol1, ...) must match Create.swift's share-parsing
	// loop exactly — see the comment there. The tag is never threaded back
	// through the mvm-vz status line, so both sides derive it independently
	// from position and a mismatch would mount the wrong host directory.
	if mounts[0].Tag != "vol0" || mounts[0].GuestPath != "/data" {
		t.Errorf("mounts[0] = %+v, want {vol0 /data}", mounts[0])
	}
	if mounts[1].Tag != "vol1" || mounts[1].GuestPath != "/app/lib" {
		t.Errorf("mounts[1] = %+v, want {vol1 /app/lib}", mounts[1])
	}
}

func TestVirtiofsMountsEmpty(t *testing.T) {
	mounts, err := virtiofsMounts(nil)
	if err != nil || len(mounts) != 0 {
		t.Errorf("virtiofsMounts(nil) = %v, %v; want no mounts, no error", mounts, err)
	}
}

func TestVirtiofsMountsInvalidFormat(t *testing.T) {
	_, err := virtiofsMounts([]string{"no-colon-here"})
	if err == nil {
		t.Error("want error for a volume missing the guest-path colon")
	}
}

// TestVirtiofsMountsGuestPathIsNotShellQuoted pins the change in kind: these
// are structured values sent as mount requests, not fragments of a shell
// command. A quoted path would be taken literally by the kernel and the mount
// would land at a directory whose name contains quote characters.
func TestVirtiofsMountsGuestPathIsNotShellQuoted(t *testing.T) {
	mounts, err := virtiofsMounts([]string{"/host/a:/data"})
	if err != nil {
		t.Fatalf("virtiofsMounts: %v", err)
	}
	if strings.ContainsAny(mounts[0].GuestPath, "'\"") {
		t.Errorf("GuestPath = %q, must be the raw path with no shell quoting", mounts[0].GuestPath)
	}
}

// resolveApplevzKernel / imageFileName / resolveAppleVZImage tests moved to
// internal/vm/applevz_test.go alongside their implementations (vm.ResolveKernel /
// vm.ImageFileName / vm.ResolveImage).

// === validateStartRM ===

func TestValidateStartRMRejectsFlag(t *testing.T) {
	err := validateStartRM(true)
	if err == nil {
		t.Fatal("validateStartRM(true) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "mvm run") {
		t.Errorf("error should point at mvm run, got: %v", err)
	}
}

func TestValidateStartRMAllowsDefault(t *testing.T) {
	if err := validateStartRM(false); err != nil {
		t.Errorf("validateStartRM(false) = %v, want nil", err)
	}
}

// === resolveOutputMode ===

func TestResolveOutputModeDefaultHuman(t *testing.T) {
	if got := resolveOutputMode(false, false); got != outHuman {
		t.Errorf("resolveOutputMode(false, false) = %v, want outHuman", got)
	}
}

func TestResolveOutputModeJSON(t *testing.T) {
	if got := resolveOutputMode(true, false); got != outJSON {
		t.Errorf("resolveOutputMode(true, false) = %v, want outJSON", got)
	}
}

func TestResolveOutputModeQuietWinsOverJSON(t *testing.T) {
	if got := resolveOutputMode(true, true); got != outQuiet {
		t.Errorf("resolveOutputMode(true, true) = %v, want outQuiet (quiet takes precedence)", got)
	}
}

// === runStart quiet mode (firecracker/daemon path) ===

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// fakeDaemonForCreate starts an httptest server implementing just enough
// of the daemon's HTTP surface (GET /health, POST /vms) for
// requireDaemon()+runStartViaDaemon to succeed against it.
func fakeDaemonForCreate(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMResponse{Name: "web", Status: "running", GuestIP: "10.0.0.2"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunStartQuietSuppressesDaemonBanner(t *testing.T) {
	srv := fakeDaemonForCreate(t)
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	out := captureStdout(t, func() {
		if err := runStart(store, "web", true, nil, "open", nil, "", "", 0, 0, "", false, nil, nil, true, nil); err != nil {
			t.Fatalf("runStart: %v", err)
		}
	})
	if strings.Contains(out, "is running!") {
		t.Errorf("quiet runStart printed the boot banner: %q", out)
	}
}

func TestRunStartNotQuietPrintsDaemonBanner(t *testing.T) {
	srv := fakeDaemonForCreate(t)
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	out := captureStdout(t, func() {
		if err := runStart(store, "web", true, nil, "open", nil, "", "", 0, 0, "", false, nil, nil, false, nil); err != nil {
			t.Fatalf("runStart: %v", err)
		}
	})
	if !strings.Contains(out, "is running!") {
		t.Errorf("non-quiet runStart suppressed the boot banner (Gateway compat break): %q", out)
	}
}

// === runStartViaDaemon: secrets + the removed guards ===

func TestRunStartViaDaemonSendsSecretNamesNotValues(t *testing.T) {
	var captured server.CreateVMRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/vms":
			json.NewDecoder(r.Body).Decode(&captured)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(server.VMResponse{Name: captured.Name, Status: "running", GuestIP: "10.0.0.2"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)

	err := runStartViaDaemon("web", nil, "open", nil, "", 0, 0, "", nil, []string{"OPENAI_API_KEY"}, false, nil)
	if err != nil {
		t.Fatalf("runStartViaDaemon: %v", err)
	}
	if len(captured.Secrets) != 1 || captured.Secrets[0] != "OPENAI_API_KEY" {
		t.Errorf("captured.Secrets = %v, want [OPENAI_API_KEY]", captured.Secrets)
	}
}

func TestRunStartNoLongerRejectsStartupOrSecretsOnDaemonPath(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)

	// "OPENAI_API_KEY" isn't a real stored secret in this test's environment
	// (no MVM_SECRET_KEY, no secret store), so validateSecretsExist rejects
	// it — but critically, it must fail with a secret-not-found error, not
	// the old "not yet supported on the daemon/firecracker path" message.
	err := runStart(store, "web", true, nil, "open", nil, "", "", 0, 0, "", false, nil, []string{"OPENAI_API_KEY"}, false, nil)
	if err == nil || strings.Contains(err.Error(), "not yet supported") {
		t.Fatalf("runStart() = %v, want a secret-not-found error, not the old unsupported-path rejection", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want it to mention the missing secret", err)
	}
}

func TestStoppedApplevzVMPassesExistenceGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := state.NewStore(filepath.Join(home, ".mvm", "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")
	vm := &state.VM{Name: "box", Backend: "applevz", Status: "stopped", CreatedAt: time.Now()}
	if _, err := store.ReserveVM(vm); err != nil {
		t.Fatalf("ReserveVM: %v", err)
	}
	vmDir := filepath.Join(home, ".mvm", "vms", "box")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "rootfs.ext4"), []byte("PRESERVED"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	_, err := runStartAppleVZ(store, "box", true, nil, "open", 0, 0, nil, outQuiet, nil, nil, "")
	if err != nil && strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runStartAppleVZ() = %v, want the stopped VM allowed through as a resume", err)
	}
	data, readErr := os.ReadFile(filepath.Join(vmDir, "rootfs.ext4"))
	if readErr != nil {
		t.Fatalf("read rootfs after resume attempt: %v", readErr)
	}
	if string(data) != "PRESERVED" {
		t.Errorf("rootfs = %q, want the preserved contents untouched", data)
	}
}

func TestRunStartViaDaemonResumesStopped(t *testing.T) {
	var startCalled, createCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/vms/box", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{
			VMResponse: server.VMResponse{Name: "box", Status: "stopped", Backend: "firecracker"},
		})
	})
	mux.HandleFunc("POST /vms/box/start", func(w http.ResponseWriter, r *http.Request) {
		startCalled = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMResponse{Name: "box", Status: "running", GuestIP: "10.0.0.5"})
	})
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		createCalled = true
		w.WriteHeader(http.StatusConflict)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")
	if err := runStartViaDaemon("box", nil, "open", nil, "", 0, 0, "", nil, nil, false, nil); err != nil {
		t.Fatalf("runStartViaDaemon() = %v, want nil", err)
	}
	if !startCalled {
		t.Error("resume endpoint POST /vms/box/start was not called")
	}
	if createCalled {
		t.Error("create endpoint POST /vms was called — start should resume, not create")
	}
}

func TestRunStartViaDaemonCreatesFreshName(t *testing.T) {
	var createCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/vms/newbox", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		createCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(server.VMResponse{Name: "newbox", Status: "running", GuestIP: "10.0.0.6"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("MVM_REMOTE", srv.URL)
	t.Setenv("MVM_API_KEY", "")
	if err := runStartViaDaemon("newbox", nil, "open", nil, "", 0, 0, "", nil, nil, false, nil); err != nil {
		t.Fatalf("runStartViaDaemon() = %v, want nil", err)
	}
	if !createCalled {
		t.Error("create endpoint POST /vms was not called for a fresh name")
	}
}
