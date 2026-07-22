package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestToCFContainersListShape(t *testing.T) {
	vms := []server.VMResponse{{
		Name:    "web",
		Status:  "running",
		GuestIP: "192.168.64.5",
		Ports:   []state.PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
	}}
	specs := map[string]*state.VMSpec{"web": {Image: "nginx", Cpus: 2, MemoryMB: 512}}

	got := mustJSON(t, toCFContainers(vms, specs))
	want := `[
  {
    "configuration": {
      "id": "web",
      "image": {
        "reference": "nginx"
      },
      "resources": {
        "cpus": 2,
        "memoryInBytes": 536870912
      },
      "publishedPorts": [
        {
          "hostPort": 8080,
          "proto": "tcp"
        }
      ]
    },
    "status": "running",
    "networks": [
      {
        "ipv4Address": "192.168.64.5"
      }
    ]
  }
]`
	if got != want {
		t.Fatalf("list shape mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToCFContainersEmptyIsArrayNotNull(t *testing.T) {
	if got := mustJSON(t, toCFContainers(nil, nil)); got != "[]" {
		t.Fatalf("empty list: got %q want []", got)
	}
}

func TestToCFContainerDefaultImageAndNoNetwork(t *testing.T) {
	got := toCFContainer(server.VMResponse{Name: "x", Status: "stopped"}, nil, false)
	if got.Configuration.Image.Reference != "base" {
		t.Fatalf("default image: got %q want base", got.Configuration.Image.Reference)
	}
	if got.Configuration.Resources.MemoryInBytes != 0 {
		t.Fatalf("nil spec memory: got %d want 0", got.Configuration.Resources.MemoryInBytes)
	}
	if len(got.Networks) != 0 {
		t.Fatalf("no-ip networks: got %d want 0", len(got.Networks))
	}
	if got.Configuration.Platform != nil {
		t.Fatalf("list path must not set platform")
	}
}

func TestToCFContainerInspectAddsPlatformAndStartedDate(t *testing.T) {
	vm := server.VMResponse{
		Name:      "web",
		Status:    "running",
		GuestIP:   "192.168.64.5",
		CreatedAt: time.Unix(1700000000, 0),
	}
	got := mustJSON(t, []cfContainer{toCFContainer(vm, &state.VMSpec{Image: "nginx", Cpus: 1, MemoryMB: 256}, true)})
	want := `[
  {
    "configuration": {
      "id": "web",
      "image": {
        "reference": "nginx"
      },
      "resources": {
        "cpus": 1,
        "memoryInBytes": 268435456
      },
      "publishedPorts": [],
      "platform": {
        "os": "linux",
        "architecture": "arm64"
      }
    },
    "status": "running",
    "networks": [
      {
        "ipv4Address": "192.168.64.5"
      }
    ],
    "startedDate": 721692800
  }
]`
	if got != want {
		t.Fatalf("inspect shape mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToCFStatsShape(t *testing.T) {
	src := []cfStatSource{{
		Name:             "web",
		CPUUsageUsec:     12500000,
		MemoryUsageBytes: 104857600,
		MemoryLimitBytes: 536870912,
		NumProcesses:     1,
		Status:           "running",
	}}
	got := mustJSON(t, toCFStats(src))
	want := `[
  {
    "id": "web",
    "cpuUsageUsec": 12500000,
    "memoryUsageBytes": 104857600,
    "memoryLimitBytes": 536870912,
    "numProcesses": 1
  }
]`
	if got != want {
		t.Fatalf("stats shape mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToCFStatsEmptyIsArrayNotNull(t *testing.T) {
	if got := mustJSON(t, toCFStats(nil)); got != "[]" {
		t.Fatalf("empty stats: got %q want []", got)
	}
}

func TestDefaultNetworkFirecracker(t *testing.T) {
	got := mustJSON(t, defaultNetwork("firecracker"))
	want := `{
  "id": "default",
  "state": "running",
  "config": {
    "mode": "nat"
  },
  "status": {
    "ipv4Subnet": "172.16.0.0/24"
  }
}`
	if got != want {
		t.Errorf("firecracker default network:\n got:\n%s\n want:\n%s", got, want)
	}
}

func TestDefaultNetworkApplevzHasNoSubnet(t *testing.T) {
	n := defaultNetwork("applevz")
	if n.Config.Mode != "nat" || n.ID != "default" {
		t.Errorf("applevz network = %+v, want id=default mode=nat", n)
	}
	if n.Status.IPv4Subnet != "" {
		t.Errorf("applevz ipv4Subnet = %q, want empty (Apple NAT assigns it dynamically)", n.Status.IPv4Subnet)
	}
}
