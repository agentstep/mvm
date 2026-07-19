package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

func TestParsePorts(t *testing.T) {
	tests := []struct {
		input     []string
		wantLen   int
		wantErr   bool
		wantHost  int
		wantGuest int
		wantProto string
	}{
		{[]string{"8080:80"}, 1, false, 8080, 80, "tcp"},
		{[]string{"3000:3000"}, 1, false, 3000, 3000, "tcp"},
		{[]string{"53:53/udp"}, 1, false, 53, 53, "udp"},
		{[]string{"8080:80", "3000:3000"}, 2, false, 8080, 80, "tcp"},
		{nil, 0, false, 0, 0, ""},
		{[]string{}, 0, false, 0, 0, ""},
		{[]string{"invalid"}, 0, true, 0, 0, ""},
		{[]string{"abc:80"}, 0, true, 0, 0, ""},
		{[]string{"8080:abc"}, 0, true, 0, 0, ""},
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

// === virtiofsMountCommands ===

func TestVirtiofsMountCommands(t *testing.T) {
	cmds, err := virtiofsMountCommands([]string{"/host/a:/data", "/host/b:/app/lib"})
	if err != nil {
		t.Fatalf("virtiofsMountCommands: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2", len(cmds))
	}
	// Tag order (vol0, vol1, ...) must match Create.swift's share-parsing
	// loop exactly — see the comment there.
	if !strings.Contains(cmds[0], "vol0") || !strings.Contains(cmds[0], "/data") {
		t.Errorf("cmds[0] = %q, want references to vol0 and /data", cmds[0])
	}
	if !strings.Contains(cmds[1], "vol1") || !strings.Contains(cmds[1], "/app/lib") {
		t.Errorf("cmds[1] = %q, want references to vol1 and /app/lib", cmds[1])
	}
}

func TestVirtiofsMountCommandsEmpty(t *testing.T) {
	cmds, err := virtiofsMountCommands(nil)
	if err != nil || len(cmds) != 0 {
		t.Errorf("virtiofsMountCommands(nil) = %v, %v; want no commands, no error", cmds, err)
	}
}

func TestVirtiofsMountCommandsInvalidFormat(t *testing.T) {
	_, err := virtiofsMountCommands([]string{"no-colon-here"})
	if err == nil {
		t.Error("want error for a volume missing the guest-path colon")
	}
}

// === resolveApplevzKernel ===

func TestResolveApplevzKernelPrefersCustomKernel(t *testing.T) {
	cacheDir := t.TempDir()
	custom := filepath.Join(cacheDir, "vmlinux-applevz")
	if err := os.WriteFile(custom, []byte("fake kernel"), 0o644); err != nil {
		t.Fatalf("write fake custom kernel: %v", err)
	}
	// Shared kernel present too, to confirm the custom one still wins.
	if err := os.WriteFile(filepath.Join(cacheDir, "vmlinux"), []byte("fake shared kernel"), 0o644); err != nil {
		t.Fatalf("write fake shared kernel: %v", err)
	}

	kernelPath, warning := resolveApplevzKernel(cacheDir)
	if kernelPath != custom {
		t.Errorf("kernelPath = %q, want %q", kernelPath, custom)
	}
	if warning != "" {
		t.Errorf("warning = %q, want empty when custom kernel exists", warning)
	}
}

func TestResolveApplevzKernelFallsBackWhenCustomKernelMissing(t *testing.T) {
	cacheDir := t.TempDir()
	shared := filepath.Join(cacheDir, "vmlinux")
	if err := os.WriteFile(shared, []byte("fake shared kernel"), 0o644); err != nil {
		t.Fatalf("write fake shared kernel: %v", err)
	}

	kernelPath, warning := resolveApplevzKernel(cacheDir)
	if kernelPath != shared {
		t.Errorf("kernelPath = %q, want %q (fallback to shared vmlinux)", kernelPath, shared)
	}
	if warning == "" {
		t.Error("warning = \"\", want a non-empty warning when falling back")
	}
	if !strings.Contains(warning, "vmlinux-applevz") {
		t.Errorf("warning = %q, want it to mention the missing custom kernel path", warning)
	}
}
