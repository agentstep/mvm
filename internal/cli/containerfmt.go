package cli

import (
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

// Container-CLI-compatible presentation shapes. The CLI transforms mvm's native
// structs into these before printing under --format json / inspect, so tooling
// built for Apple `container` consumes mvm's native output unchanged.

type cfPort struct {
	HostPort int    `json:"hostPort"`
	Proto    string `json:"proto"`
}
type cfImageRef struct {
	Reference string `json:"reference"`
}
type cfResources struct {
	Cpus          int   `json:"cpus"`
	MemoryInBytes int64 `json:"memoryInBytes"`
}
type cfPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}
type cfConfiguration struct {
	ID             string      `json:"id"`
	Image          cfImageRef  `json:"image"`
	Resources      cfResources `json:"resources"`
	PublishedPorts []cfPort    `json:"publishedPorts"`
	Platform       *cfPlatform `json:"platform,omitempty"` // inspect only
}
type cfNetwork struct {
	IPv4Address string `json:"ipv4Address"`
}
type cfContainer struct {
	Configuration cfConfiguration `json:"configuration"`
	Status        string          `json:"status"`
	Networks      []cfNetwork     `json:"networks"`
	StartedDate   float64         `json:"startedDate,omitempty"` // inspect only
}
type cfStats struct {
	ID               string `json:"id"`
	CPUUsageUsec     uint64 `json:"cpuUsageUsec"` // cumulative microseconds, monotonic
	MemoryUsageBytes uint64 `json:"memoryUsageBytes"`
	MemoryLimitBytes uint64 `json:"memoryLimitBytes"`
	NumProcesses     uint32 `json:"numProcesses"`
}
type cfDescriptor struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}
type cfImage struct {
	Reference  string       `json:"reference"`
	Descriptor cfDescriptor `json:"descriptor"`
}
type cfDiskEntry struct {
	Active      uint64 `json:"active"`
	Reclaimable uint64 `json:"reclaimable"`
	SizeInBytes uint64 `json:"sizeInBytes"`
	Total       uint64 `json:"total"`
}
type cfDiskUsage struct {
	Containers cfDiskEntry `json:"containers"`
	Images     cfDiskEntry `json:"images"`
	Volumes    cfDiskEntry `json:"volumes"`
}

// cfEpochOffset converts a Unix timestamp to Apple's CoreFoundation epoch
// (seconds since 2001-01-01 UTC), the base container's startedDate uses.
const cfEpochOffset = 978307200

const (
	cfOS   = "linux"
	cfArch = "arm64"
)

// cfStatSource is the CLI-side, pre-transform per-VM stats record. It exists
// because server.VMStats (a frozen wire contract that must not change) carries
// only an instantaneous %cpu, whereas cfStats needs cumulative CPU microseconds,
// byte units, a memory limit, and a process count. Status/Backend/PID are
// display-only (the human table) and intentionally absent from cfStats JSON.
type cfStatSource struct {
	Name             string
	CPUUsageUsec     uint64
	MemoryUsageBytes uint64
	MemoryLimitBytes uint64
	NumProcesses     uint32
	Status           string
	Backend          string
	PID              int
}

func imageRef(spec *state.VMSpec) string {
	if spec == nil || spec.Image == "" {
		return "base"
	}
	return spec.Image
}

func cfResourcesFrom(spec *state.VMSpec) cfResources {
	if spec == nil {
		return cfResources{}
	}
	return cfResources{
		Cpus:          spec.Cpus,
		MemoryInBytes: int64(spec.MemoryMB) * 1024 * 1024,
	}
}

func cfPortsFrom(ports []state.PortMap) []cfPort {
	out := make([]cfPort, 0, len(ports))
	for _, p := range ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		out = append(out, cfPort{HostPort: p.HostPort, Proto: proto})
	}
	return out
}

func cfNetworksFrom(guestIP string) []cfNetwork {
	if guestIP == "" {
		return []cfNetwork{}
	}
	return []cfNetwork{{IPv4Address: guestIP}}
}

// toCFContainer converts one native VMResponse (plus its persisted spec, which
// carries the image/cpus/memory that VMResponse omits) into container's
// cfContainer shape. spec may be nil. When inspect is true the inspect-only
// fields (platform, startedDate) are populated.
func toCFContainer(vm server.VMResponse, spec *state.VMSpec, inspect bool) cfContainer {
	c := cfContainer{
		Configuration: cfConfiguration{
			ID:             vm.Name,
			Image:          cfImageRef{Reference: imageRef(spec)},
			Resources:      cfResourcesFrom(spec),
			PublishedPorts: cfPortsFrom(vm.Ports),
		},
		Status:   vm.Status,
		Networks: cfNetworksFrom(vm.GuestIP),
	}
	if inspect {
		c.Configuration.Platform = &cfPlatform{OS: cfOS, Architecture: cfArch}
		if !vm.CreatedAt.IsZero() {
			c.StartedDate = float64(vm.CreatedAt.UTC().Unix() - cfEpochOffset)
		}
	}
	return c
}

// toCFContainers transforms a list of native VMs into cfContainers (list path,
// no platform/startedDate). Always non-nil so the empty case marshals to `[]`.
func toCFContainers(vms []server.VMResponse, specs map[string]*state.VMSpec) []cfContainer {
	out := make([]cfContainer, 0, len(vms))
	for _, vm := range vms {
		out = append(out, toCFContainer(vm, specs[vm.Name], false))
	}
	return out
}

// toCFStats transforms CLI-local stat sources into the flat container cfStats
// shape. Non-nil slice for the empty case.
func toCFStats(src []cfStatSource) []cfStats {
	out := make([]cfStats, 0, len(src))
	for _, s := range src {
		out = append(out, cfStats{
			ID:               s.Name,
			CPUUsageUsec:     s.CPUUsageUsec,
			MemoryUsageBytes: s.MemoryUsageBytes,
			MemoryLimitBytes: s.MemoryLimitBytes,
			NumProcesses:     s.NumProcesses,
		})
	}
	return out
}
