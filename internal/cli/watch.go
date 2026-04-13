package cli

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// watchDirectory polls a directory for changes and returns when files change.
// Returns the new hash, or error.
func watchDirectory(dir string, interval time.Duration, prevHash string) (string, error) {
	for {
		hash, err := hashDirectory(dir)
		if err != nil {
			return "", err
		}
		if hash != prevHash {
			return hash, nil
		}
		time.Sleep(interval)
	}
}

// hashDirectory computes a quick hash of all file modification times in a directory.
func hashDirectory(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable files
		}
		// Skip hidden dirs and common noise
		name := d.Name()
		if d.IsDir() && (name == ".git" || name == "node_modules" || name == "__pycache__" || name == ".venv") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return nil
			}
			fmt.Fprintf(h, "%s:%d\n", path, info.ModTime().UnixNano())
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// isDirectory checks if a path is a directory.
func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
