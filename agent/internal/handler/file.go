package handler

import (
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
