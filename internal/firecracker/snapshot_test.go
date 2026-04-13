package firecracker

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveKeyDeterministic(t *testing.T) {
	k1 := deriveKey("mysecret")
	k2 := deriveKey("mysecret")
	if !bytes.Equal(k1, k2) {
		t.Error("deriveKey should be deterministic")
	}
	if len(k1) != 32 {
		t.Errorf("key length = %d, want 32", len(k1))
	}
}

func TestDeriveKeyDifferentInputs(t *testing.T) {
	k1 := deriveKey("secret1")
	k2 := deriveKey("secret2")
	if bytes.Equal(k1, k2) {
		t.Error("different inputs should produce different keys")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := deriveKey("testkey")
	plaintext := []byte("hello world, this is a test of AES-256-GCM encryption")

	encrypted, err := encryptAESGCM(plaintext, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// Encrypted should be different from plaintext
	if bytes.Equal(encrypted, plaintext) {
		t.Error("encrypted should differ from plaintext")
	}

	// Encrypted should be longer (nonce + tag overhead)
	if len(encrypted) <= len(plaintext) {
		t.Errorf("encrypted len %d should be > plaintext len %d", len(encrypted), len(plaintext))
	}

	decrypted, err := decryptAESGCM(encrypted, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted = %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1 := deriveKey("correct")
	key2 := deriveKey("wrong")

	encrypted, _ := encryptAESGCM([]byte("secret data"), key1)

	_, err := decryptAESGCM(encrypted, key2)
	if err == nil {
		t.Error("decrypt with wrong key should fail")
	}
}

func TestDecryptTruncated(t *testing.T) {
	_, err := decryptAESGCM([]byte("short"), deriveKey("key"))
	if err == nil {
		t.Error("decrypt truncated data should fail")
	}
}

func TestEncryptDecryptLargeData(t *testing.T) {
	key := deriveKey("bigdata")
	// 1MB of data
	plaintext := make([]byte, 1024*1024)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	encrypted, err := encryptAESGCM(plaintext, key)
	if err != nil {
		t.Fatalf("encrypt large: %v", err)
	}

	decrypted, err := decryptAESGCM(encrypted, key)
	if err != nil {
		t.Fatalf("decrypt large: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Error("large data round-trip failed")
	}
}

func TestEncryptedDifferentEachTime(t *testing.T) {
	key := deriveKey("nonce")
	plaintext := []byte("same input")

	enc1, _ := encryptAESGCM(plaintext, key)
	enc2, _ := encryptAESGCM(plaintext, key)

	if bytes.Equal(enc1, enc2) {
		t.Error("encrypting same plaintext twice should produce different ciphertext (random nonce)")
	}
}

// === NEW TESTS: File encryption/decryption ===

func TestEncryptDecryptFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "test.bin")
	plaintext := []byte("This is sensitive snapshot data")

	os.WriteFile(plainPath, plaintext, 0o600)

	key := "my-snapshot-key"
	if err := encryptFile(plainPath, key); err != nil {
		t.Fatalf("encryptFile: %v", err)
	}

	// Original should be removed
	if _, err := os.Stat(plainPath); !os.IsNotExist(err) {
		t.Error("plaintext file should be removed after encryption")
	}

	// Encrypted file should exist
	encPath := plainPath + ".enc"
	if _, err := os.Stat(encPath); os.IsNotExist(err) {
		t.Error("encrypted file should exist")
	}

	// Encrypted should not equal plaintext
	encData, _ := os.ReadFile(encPath)
	if bytes.Equal(encData, plaintext) {
		t.Error("encrypted data should differ from plaintext")
	}

	// Decrypt
	decryptedPath := filepath.Join(dir, "decrypted.bin")
	if err := decryptFile(encPath, decryptedPath, key); err != nil {
		t.Fatalf("decryptFile: %v", err)
	}

	decrypted, _ := os.ReadFile(decryptedPath)
	if !bytes.Equal(decrypted, plaintext) {
		t.Error("decrypted data should match original")
	}
}

func TestDecryptFileWrongKey(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "secret.bin")
	os.WriteFile(plainPath, []byte("secret data"), 0o600)

	encryptFile(plainPath, "correctkey")

	decryptedPath := filepath.Join(dir, "decrypted.bin")
	err := decryptFile(plainPath+".enc", decryptedPath, "wrongkey")
	if err == nil {
		t.Error("decrypting with wrong key should fail")
	}
}

func TestEncryptFilePermissions(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "perms.bin")
	os.WriteFile(plainPath, []byte("data"), 0o600)

	encryptFile(plainPath, "key")

	info, err := os.Stat(plainPath + ".enc")
	if err != nil {
		t.Fatalf("stat encrypted: %v", err)
	}
	// Should be 0600 (owner read/write only)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("encrypted file mode = %o, want 600", info.Mode().Perm())
	}
}

// === NEW TEST: ListSnapshots ===

func TestListSnapshotsEmpty(t *testing.T) {
	dir := t.TempDir()
	snaps, err := ListSnapshots(dir)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("should have no snapshots, got %d", len(snaps))
	}
}

func TestListSnapshotsNonexistentDir(t *testing.T) {
	snaps, err := ListSnapshots("/nonexistent/path")
	if err != nil {
		t.Fatalf("should not error on nonexistent dir: %v", err)
	}
	if snaps != nil {
		t.Error("should return nil for nonexistent dir")
	}
}

func TestListSnapshotsFindsValid(t *testing.T) {
	dir := t.TempDir()

	// Create valid snapshot directories (with meta.json)
	for _, name := range []string{"snap1", "snap2"} {
		snapDir := filepath.Join(dir, name)
		os.MkdirAll(snapDir, 0o755)
		os.WriteFile(filepath.Join(snapDir, "meta.json"), []byte(`{"vm":"test"}`), 0o644)
	}

	// Create a non-snapshot directory (no meta.json)
	os.MkdirAll(filepath.Join(dir, "not-a-snapshot"), 0o755)

	// Create a file (not a directory)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hi"), 0o644)

	snaps, err := ListSnapshots(dir)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("expected 2 snapshots, got %d: %v", len(snaps), snaps)
	}
}

// === NEW TEST: SnapshotKeyEnvVar constant ===

func TestSnapshotKeyEnvVar(t *testing.T) {
	if SnapshotKeyEnvVar != "MVM_SNAPSHOT_KEY" {
		t.Errorf("SnapshotKeyEnvVar = %q, want MVM_SNAPSHOT_KEY", SnapshotKeyEnvVar)
	}
}

// === NEW TEST: deriveKey produces 256-bit (32 byte) key ===

func TestDeriveKeyLength(t *testing.T) {
	for _, input := range []string{"", "x", "a long passphrase with many words"} {
		key := deriveKey(input)
		if len(key) != 32 {
			t.Errorf("deriveKey(%q) length = %d, want 32 (AES-256)", input, len(key))
		}
	}
}

// === NEW TEST: Empty data encryption ===

func TestEncryptDecryptEmptyData(t *testing.T) {
	key := deriveKey("empty")
	encrypted, err := encryptAESGCM([]byte{}, key)
	if err != nil {
		t.Fatalf("encrypt empty: %v", err)
	}
	// Even empty plaintext should produce ciphertext (nonce + auth tag)
	if len(encrypted) == 0 {
		t.Error("encrypted empty data should not be empty (nonce + tag)")
	}

	decrypted, err := decryptAESGCM(encrypted, key)
	if err != nil {
		t.Fatalf("decrypt empty: %v", err)
	}
	if len(decrypted) != 0 {
		t.Error("decrypted should be empty")
	}
}
