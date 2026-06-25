package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(KeyEnvVar, "") // force keyfile generation
	s := New(dir)

	if err := s.Put("API_KEY", "sk-secret-value"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-secret-value" {
		t.Fatalf("Get = %q, want sk-secret-value", got)
	}

	// Plaintext must not appear in the on-disk store.
	raw, err := os.ReadFile(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if contains(string(raw), "sk-secret-value") {
		t.Fatal("plaintext secret value found in on-disk store")
	}

	// Key file must be 0600.
	info, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestListNamesOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(KeyEnvVar, "")
	s := New(dir)
	_ = s.Put("B", "2")
	_ = s.Put("A", "1")
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 || names[0] != "A" || names[1] != "B" {
		t.Fatalf("List = %v, want [A B] sorted", names)
	}
}

func TestDeleteAndHas(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(KeyEnvVar, "")
	s := New(dir)
	_ = s.Put("X", "v")
	if ok, _ := s.Has("X"); !ok {
		t.Fatal("Has(X) = false after Put")
	}
	if err := s.Delete("X"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := s.Has("X"); ok {
		t.Fatal("Has(X) = true after Delete")
	}
	if _, err := s.Get("X"); err == nil {
		t.Fatal("Get(X) succeeded after Delete")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
