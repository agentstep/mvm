package firecracker

import (
	"io"
	"os"
	"path/filepath"
)

func writeFileWithPerms(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func execStdout() io.Writer { return os.Stdout }
