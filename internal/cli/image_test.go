package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
)

func TestImagesToCF(t *testing.T) {
	got := imagesToCF([]server.ImageInfo{{Name: "web", SizeMB: 128}, {Name: "db", SizeMB: 0}})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Reference != "web" || got[0].Descriptor.Size != 128*1024*1024 || got[0].Descriptor.Digest != "" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Descriptor.Size != 0 {
		t.Errorf("zero-MiB size = %d, want 0", got[1].Descriptor.Size)
	}
}

func TestImagesToCFEmptyMarshalsToArray(t *testing.T) {
	b, _ := json.Marshal(imagesToCF(nil))
	if string(b) != "[]" {
		t.Errorf("marshal(nil) = %s, want []", b)
	}
}

func TestFindImage(t *testing.T) {
	imgs := []server.ImageInfo{{Name: "web", SizeMB: 64}}
	got, err := findImage(imgs, "web")
	if err != nil {
		t.Fatal(err)
	}
	cf := imageToCF(got)
	if cf.Reference != "web" || cf.Descriptor.Size != 64*1024*1024 {
		t.Errorf("cf = %+v", cf)
	}
	if _, err := findImage(imgs, "missing"); err == nil {
		t.Error("expected error for missing image")
	}
}

func TestUnreferencedImages(t *testing.T) {
	imgs := []server.ImageInfo{{Name: "used"}, {Name: "orphan"}}
	vms := []*state.VM{
		{Name: "vm1", Spec: &state.VMSpec{Image: "used"}},
		{Name: "vm2", Spec: nil},
	}
	got := unreferencedImages(imgs, vms)
	if len(got) != 1 || got[0] != "orphan" {
		t.Fatalf("unreferenced = %v, want [orphan]", got)
	}
}

func TestImageCmdWiring(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	c := newImageCmd(store)
	if c.Use != "image" {
		t.Fatalf("Use = %q, want image", c.Use)
	}
	names := map[string]bool{}
	for _, sub := range c.Commands() {
		names[sub.Name()] = true
	}
	if !names["ls"] || !names["rm"] || !names["inspect"] {
		t.Fatalf("subcommands = %v, want ls+rm+inspect", names)
	}
	ls, _, err := c.Find([]string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	if ls.Flags().Lookup("format") == nil {
		t.Error("ls missing --format")
	}
}
