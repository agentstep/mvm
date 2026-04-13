package protocol

import (
	"bytes"
	"testing"
)

func TestWriteReadFrame(t *testing.T) {
	req := Request{
		Type: ReqExec,
		ID:   "test-1",
		Exec: &ExecRequest{
			Command: "echo hello",
			Env:     map[string]string{"FOO": "bar"},
			WorkDir: "/tmp",
		},
	}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, req); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	var got Request
	if err := ReadFrame(&buf, &got); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}

	if got.Type != ReqExec {
		t.Errorf("Type = %q, want %q", got.Type, ReqExec)
	}
	if got.ID != "test-1" {
		t.Errorf("ID = %q, want %q", got.ID, "test-1")
	}
	if got.Exec == nil {
		t.Fatal("Exec is nil")
	}
	if got.Exec.Command != "echo hello" {
		t.Errorf("Command = %q, want %q", got.Exec.Command, "echo hello")
	}
	if got.Exec.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want %q", got.Exec.Env["FOO"], "bar")
	}
}

func TestWriteReadResponse(t *testing.T) {
	resp := Response{
		Type:     RespExit,
		ID:       "test-2",
		Data:     []byte("hello world\n"),
		ExitCode: 0,
	}

	var buf bytes.Buffer
	WriteFrame(&buf, resp)

	var got Response
	ReadFrame(&buf, &got)

	if got.Type != RespExit {
		t.Errorf("Type = %q, want %q", got.Type, RespExit)
	}
	if string(got.Data) != "hello world\n" {
		t.Errorf("Data = %q", got.Data)
	}
	if got.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", got.ExitCode)
	}
}

func TestMultipleFrames(t *testing.T) {
	var buf bytes.Buffer

	// Write 3 frames
	for i := 0; i < 3; i++ {
		WriteFrame(&buf, Response{Type: RespOK, ID: "multi"})
	}

	// Read 3 frames
	for i := 0; i < 3; i++ {
		var resp Response
		if err := ReadFrame(&buf, &resp); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if resp.Type != RespOK {
			t.Errorf("frame %d: Type = %q", i, resp.Type)
		}
	}
}

func TestPingRequest(t *testing.T) {
	req := Request{Type: ReqPing, ID: "ping-1"}

	var buf bytes.Buffer
	WriteFrame(&buf, req)

	var got Request
	ReadFrame(&buf, &got)

	if got.Type != ReqPing {
		t.Errorf("Type = %q, want %q", got.Type, ReqPing)
	}
}
