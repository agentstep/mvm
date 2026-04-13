package cli

import (
	"testing"
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
