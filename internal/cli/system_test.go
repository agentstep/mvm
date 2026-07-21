package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func testSystemCmd(t *testing.T) *cobra.Command {
	t.Helper()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	return newSystemCmd(lima.NewClient("mvm"), store, "1.2.3", "abc123", "2026-07-21")
}

func TestSystemSubcommandsRegistered(t *testing.T) {
	c := testSystemCmd(t)
	if c.Use != "system" {
		t.Fatalf("Use = %q, want system", c.Use)
	}
	have := map[string]bool{}
	for _, sub := range c.Commands() {
		have[sub.Name()] = true
	}
	// Task 17 wires these; status/df are added by Tasks 18/19 (which
	// extend this test to assert them).
	for _, w := range []string{"version", "logs", "start", "stop", "install", "uninstall", "status", "df"} {
		if !have[w] {
			t.Errorf("missing subcommand %q (have %v)", w, have)
		}
	}
}

func TestSystemVersionPrints(t *testing.T) {
	c := testSystemCmd(t)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{"version"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "1.2.3") {
		t.Errorf("output %q missing version 1.2.3", buf.String())
	}
}

func TestBuildSystemStatusFirecrackerRunning(t *testing.T) {
	txt := renderSystemStatusText(buildSystemStatus("firecracker", true, "/root/.mvm/server.sock", "1.2.3"))
	for _, sub := range []string{"is running", "container-apiserver version: ", "application install root: "} {
		if !strings.Contains(txt, sub) {
			t.Errorf("text missing %q:\n%s", sub, txt)
		}
	}
}

func TestBuildSystemStatusFirecrackerStopped(t *testing.T) {
	s := buildSystemStatus("firecracker", false, "", "1.2.3")
	if s.DaemonRunning {
		t.Error("DaemonRunning = true, want false")
	}
	if strings.Contains(renderSystemStatusText(s), "is running") {
		t.Error("stopped daemon should not report \"is running\"")
	}
}

func TestBuildSystemStatusApplevz(t *testing.T) {
	txt := renderSystemStatusText(buildSystemStatus("applevz", false, "", "1.2.3"))
	if !strings.Contains(txt, "applevz backend — no daemon required") {
		t.Errorf("applevz text missing marker:\n%s", txt)
	}
}

func TestSystemStatusJSONShape(t *testing.T) {
	b, _ := json.Marshal(buildSystemStatus("firecracker", true, "/s.sock", "1.2.3"))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"backend", "daemonRunning"} {
		if _, ok := m[k]; !ok {
			t.Errorf("json missing key %q (%s)", k, b)
		}
	}
}

func TestBuildDiskUsageComputes(t *testing.T) {
	du := buildDiskUsage(
		[]resourceItem{{InUse: true, Bytes: 100}, {InUse: false, Bytes: 40}},
		[]resourceItem{{InUse: true, Bytes: 10}, {InUse: false, Bytes: 5}},
	)
	if du.Containers.Active != 1 || du.Containers.Total != 2 {
		t.Errorf("containers active/total = %d/%d, want 1/2", du.Containers.Active, du.Containers.Total)
	}
	if du.Containers.SizeInBytes != 140 || du.Containers.Reclaimable != 40 {
		t.Errorf("containers size/reclaimable = %d/%d, want 140/40", du.Containers.SizeInBytes, du.Containers.Reclaimable)
	}
	if du.Images.SizeInBytes != 15 || du.Images.Reclaimable != 5 {
		t.Errorf("images size/reclaimable = %d/%d, want 15/5", du.Images.SizeInBytes, du.Images.Reclaimable)
	}
	if du.Volumes != (cfDiskEntry{}) {
		t.Errorf("volumes = %+v, want zero until Slice 2", du.Volumes)
	}
}

func TestSystemDFJSONShape(t *testing.T) {
	b, _ := json.Marshal(buildDiskUsage(nil, nil))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"containers", "images", "volumes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("df json missing %q (%s)", k, b)
		}
	}
}
