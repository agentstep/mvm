package handler

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/agent/internal/protocol"
)

// HandleWriteFile writes content to a file.
func HandleWriteFile(req *protocol.FileRequest) *protocol.Response {
	if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}

	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}

	if err := os.WriteFile(req.Path, req.Content, mode); err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}

	return &protocol.Response{Type: protocol.RespOK}
}

// HandleReadFile reads a file and returns its contents.
func HandleReadFile(req *protocol.FileRequest) *protocol.Response {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}

	return &protocol.Response{Type: protocol.RespOK, Data: data}
}

// DirEntry describes one entry returned by HandleListDir.
type DirEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
	IsDir bool   `json:"is_dir"`
}

// HandleListDir lists a directory.
//
// Returns entries rather than shelling out to `ls`: parsing ls output is
// fragile, and the guest image ships a minimal userland where the flags
// available cannot be assumed.
func HandleListDir(req *protocol.FileRequest) *protocol.Response {
	entries, err := os.ReadDir(req.Path)
	if err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		de := DirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if info, err := e.Info(); err == nil {
			de.Size = info.Size()
			de.Mode = info.Mode().String()
		}
		out = append(out, de)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}
	return &protocol.Response{Type: protocol.RespOK, Data: data}
}

// HandleDeleteFile removes a file or an empty directory.
//
// Deliberately not recursive: a recursive delete driven from outside the guest
// is a foot-gun with no undo, and anything genuinely needing one can run `rm -r`
// through exec where the intent is explicit.
func HandleDeleteFile(req *protocol.FileRequest) *protocol.Response {
	if err := os.Remove(req.Path); err != nil {
		return &protocol.Response{Type: protocol.RespError, Error: err.Error()}
	}
	return &protocol.Response{Type: protocol.RespOK}
}
