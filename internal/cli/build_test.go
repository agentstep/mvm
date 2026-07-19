package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentstep/mvm/internal/state"
)

func TestRunBuildDownloadsToAppleVZCacheAfterBuild(t *testing.T) {
	dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("RUN echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	downloadHit := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/build":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"image": "my-image", "status": "built"})
		case r.URL.Path == "/v1/images/my-image/download":
			downloadHit = true
			w.Write([]byte("fake-ext4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "applevz")

	if err := runBuild(store, dockerfile, "my-image", 512); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	if !downloadHit {
		t.Error("runBuild did not fetch the built image into the applevz cache")
	}
	if _, err := os.Stat(filepath.Join(home, ".mvm", "cache", "my-image.ext4")); err != nil {
		t.Errorf("image not cached locally: %v", err)
	}
}

func TestRunBuildSkipsDownloadOnFirecrackerBackend(t *testing.T) {
	dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
	os.WriteFile(dockerfile, []byte("RUN echo hi\n"), 0o644)

	downloadHit := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/build":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"image": "my-image", "status": "built"})
		case r.URL.Path == "/v1/images/my-image/download":
			downloadHit = true
			w.Write([]byte("fake-ext4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	t.Setenv("MVM_REMOTE", ts.URL)

	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.MarkInitialized("v1.13.0", "firecracker")

	if err := runBuild(store, dockerfile, "my-image", 512); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	if downloadHit {
		t.Error("runBuild fetched the image into the applevz cache on a firecracker-backend host — should be a no-op")
	}
}
