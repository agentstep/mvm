package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthMiddleware_NoHeader(t *testing.T) {
	handler := authMiddleware("secret-key", okHandler())

	req := httptest.NewRequest("GET", "/vms", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthMiddleware_WrongKey(t *testing.T) {
	handler := authMiddleware("secret-key", okHandler())

	req := httptest.NewRequest("GET", "/vms", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthMiddleware_ValidKey(t *testing.T) {
	handler := authMiddleware("secret-key", okHandler())

	req := httptest.NewRequest("GET", "/vms", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestAuthMiddleware_HealthSkipsAuth(t *testing.T) {
	handler := authMiddleware("secret-key", okHandler())

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (health should skip auth)", w.Code)
	}
}

func TestLoadAPIKey_Priority(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "api-key")
	os.WriteFile(keyFile, []byte("file-key\n"), 0o644)

	// Flag value takes highest priority.
	got := LoadAPIKey("flag-key", keyFile)
	if got != "flag-key" {
		t.Errorf("LoadAPIKey with flag = %q, want flag-key", got)
	}

	// Env var is next.
	t.Setenv("MVM_API_KEY", "env-key")
	got = LoadAPIKey("", keyFile)
	if got != "env-key" {
		t.Errorf("LoadAPIKey with env = %q, want env-key", got)
	}

	// File is next when env is unset.
	t.Setenv("MVM_API_KEY", "")
	got = LoadAPIKey("", keyFile)
	if got != "file-key" {
		t.Errorf("LoadAPIKey with file = %q, want file-key", got)
	}

	// Empty when nothing is set.
	got = LoadAPIKey("", filepath.Join(dir, "nonexistent"))
	if got != "" {
		t.Errorf("LoadAPIKey with nothing = %q, want empty", got)
	}
}
