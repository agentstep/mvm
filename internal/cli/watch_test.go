package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHashDirectory(t *testing.T) {
	dir := t.TempDir()

	// Create a file
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello"), 0o644)

	hash1, err := hashDirectory(dir)
	if err != nil {
		t.Fatalf("hashDirectory: %v", err)
	}
	if hash1 == "" {
		t.Error("hash should not be empty")
	}

	// Same content, same hash
	hash2, _ := hashDirectory(dir)
	if hash1 != hash2 {
		t.Error("hash should be stable for unchanged directory")
	}

	// Modify file
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("world"), 0o644)

	hash3, _ := hashDirectory(dir)
	if hash3 == hash1 {
		t.Error("hash should change when file is modified")
	}

	// Add file
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644)

	hash4, _ := hashDirectory(dir)
	if hash4 == hash3 {
		t.Error("hash should change when file is added")
	}
}

func TestHashDirectorySkipsGitAndNodeModules(t *testing.T) {
	dir := t.TempDir()

	// Create ignored directories
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "objects", "abc"), []byte("git object"), 0o644)

	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("module"), 0o644)

	// Create a real file
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)

	hash1, _ := hashDirectory(dir)

	// Modify git object — hash should NOT change
	os.WriteFile(filepath.Join(dir, ".git", "objects", "abc"), []byte("modified"), 0o644)
	hash2, _ := hashDirectory(dir)
	if hash1 != hash2 {
		t.Error("hash should ignore .git changes")
	}

	// Modify node_modules — hash should NOT change
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("modified"), 0o644)
	hash3, _ := hashDirectory(dir)
	if hash1 != hash3 {
		t.Error("hash should ignore node_modules changes")
	}
}

func TestIsDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "test.txt")
	os.WriteFile(file, []byte("hi"), 0o644)

	if !isDirectory(dir) {
		t.Error("should detect directory")
	}
	if isDirectory(file) {
		t.Error("should not detect file as directory")
	}
	if isDirectory("/nonexistent") {
		t.Error("should not detect nonexistent path")
	}
}
