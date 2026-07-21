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
	for _, w := range []string{"version", "logs", "start", "stop", "install", "uninstall", "status"} {
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
