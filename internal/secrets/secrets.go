// Package secrets is an encrypted-at-rest store for named secret values.
//
// Values are sealed with AES-256-GCM. The key is kept OUT of the store: it
// comes from $MVM_SECRET_KEY (base64 of 32 bytes) or a 0600 key file generated
// on first use. Plaintext is never written to the store, never logged, and
// (by the caller's contract) injected into a guest only per-exec from host
// memory — never to a guest file, and never while a memory snapshot is taken.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// KeyEnvVar is the env var holding a base64-encoded 32-byte key (optional).
const KeyEnvVar = "MVM_SECRET_KEY"

// Store persists sealed secrets under a data directory.
type Store struct {
	dir string // e.g. ~/.mvm
}

// New returns a Store rooted at dir (creating it as needed on write).
func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) storePath() string { return filepath.Join(s.dir, "secrets.json") }
func (s *Store) keyPath() string   { return filepath.Join(s.dir, "secret.key") }

// loadKey resolves the 32-byte key from $MVM_SECRET_KEY or the key file,
// generating and persisting a new key (0600) on first use.
func (s *Store) loadKey() ([]byte, error) {
	if env := os.Getenv(KeyEnvVar); env != "" {
		key, err := base64.StdEncoding.DecodeString(env)
		if err != nil {
			return nil, fmt.Errorf("%s is not valid base64: %w", KeyEnvVar, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("%s must decode to 32 bytes, got %d", KeyEnvVar, len(key))
		}
		return key, nil
	}
	if data, err := os.ReadFile(s.keyPath()); err == nil {
		key, derr := base64.StdEncoding.DecodeString(string(data))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("corrupt key file %s", s.keyPath())
		}
		return key, nil
	}
	// Generate + persist.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(s.keyPath(), []byte(enc), 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return key, nil
}

// blob is the on-disk shape: name -> base64(nonce||ciphertext).
func (s *Store) load() (map[string]string, error) {
	data, err := os.ReadFile(s.storePath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse secret store: %w", err)
	}
	return m, nil
}

func (s *Store) save(m map[string]string) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.storePath(), data, 0o600)
}

func (s *Store) seal(key, plain []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *Store) open(key []byte, b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

// Put stores (or replaces) a secret value.
func (s *Store) Put(name, value string) error {
	key, err := s.loadKey()
	if err != nil {
		return err
	}
	m, err := s.load()
	if err != nil {
		return err
	}
	sealed, err := s.seal(key, []byte(value))
	if err != nil {
		return err
	}
	m[name] = sealed
	return s.save(m)
}

// Get decrypts and returns a secret value.
func (s *Store) Get(name string) (string, error) {
	key, err := s.loadKey()
	if err != nil {
		return "", err
	}
	m, err := s.load()
	if err != nil {
		return "", err
	}
	b64, ok := m[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found", name)
	}
	plain, err := s.open(key, b64)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %q: %w", name, err)
	}
	return string(plain), nil
}

// List returns the secret names only (never values), sorted.
func (s *Store) List() ([]string, error) {
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// Has reports whether a secret exists (without decrypting it).
func (s *Store) Has(name string) (bool, error) {
	m, err := s.load()
	if err != nil {
		return false, err
	}
	_, ok := m[name]
	return ok, nil
}

// Delete removes a secret. No error if it doesn't exist.
func (s *Store) Delete(name string) error {
	m, err := s.load()
	if err != nil {
		return err
	}
	delete(m, name)
	return s.save(m)
}
