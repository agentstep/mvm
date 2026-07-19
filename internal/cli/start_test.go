package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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
