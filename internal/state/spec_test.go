package state

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestVMSpecRoundTripsThroughStore(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))

	spec := &VMSpec{
		Image:     "my-image",
		Cpus:      4,
		MemoryMB:  2048,
		Ports:     []PortMap{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}},
		Volumes:   []string{"/host:/guest"},
		NetPolicy: "deny",
		Seccomp:   "strict",
		Secrets:   []string{"OPENAI_API_KEY"},
		Startup:   json.RawMessage(`{"commands":["make dev"]}`),
	}
	vm := &VM{
		Name:      "web",
		Status:    "running",
		CreatedAt: time.Now(),
		Spec:      spec,
	}
	if err := store.AddVM(vm); err != nil {
		t.Fatalf("AddVM: %v", err)
	}

	got, err := store.GetVM("web")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.Spec == nil {
		t.Fatal("Spec = nil, want persisted spec")
	}
	// Startup is json.RawMessage — Store.Transact round-trips the whole
	// state through json.MarshalIndent, which reformats whitespace inside
	// raw JSON, so compare it as parsed JSON rather than raw bytes.
	gotStartup, wantStartup := got.Spec.Startup, spec.Startup
	got.Spec.Startup, spec.Startup = nil, nil
	if !reflect.DeepEqual(got.Spec, spec) {
		t.Errorf("Spec (excluding Startup) = %+v, want %+v", got.Spec, spec)
	}

	var gotParsed, wantParsed interface{}
	if err := json.Unmarshal(gotStartup, &gotParsed); err != nil {
		t.Fatalf("unmarshal got Startup: %v", err)
	}
	if err := json.Unmarshal(wantStartup, &wantParsed); err != nil {
		t.Fatalf("unmarshal want Startup: %v", err)
	}
	if !reflect.DeepEqual(gotParsed, wantParsed) {
		t.Errorf("Startup = %s, want %s", gotStartup, wantStartup)
	}
}

func TestVMWithoutSpecOmitsKey(t *testing.T) {
	data, err := json.Marshal(&VM{Name: "bare"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	if _, ok := m["spec"]; ok {
		t.Error(`VM without spec should omit the "spec" key (omitempty)`)
	}
}
