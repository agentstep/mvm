package cli

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// === Agent protocol: length-prefixed request format ===

func TestAgentProtocolLengthPrefix(t *testing.T) {
	// Verify the length-prefixed encoding used by runExecDirect2
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"type": "exec",
		"id":   "e",
		"exec": map[string]string{"command": "echo hello"},
	})

	length := make([]byte, 4)
	length[0] = byte(len(reqJSON) >> 24)
	length[1] = byte(len(reqJSON) >> 16)
	length[2] = byte(len(reqJSON) >> 8)
	length[3] = byte(len(reqJSON))

	// Decode length
	decoded := int(length[0])<<24 | int(length[1])<<16 | int(length[2])<<8 | int(length[3])
	if decoded != len(reqJSON) {
		t.Errorf("decoded length = %d, want %d", decoded, len(reqJSON))
	}
}

func TestAgentProtocolLengthPrefixSmall(t *testing.T) {
	// Small payload (< 256 bytes)
	payload := []byte(`{"type":"exec","id":"e","exec":{"command":"ls"}}`)
	size := len(payload)

	length := make([]byte, 4)
	length[0] = byte(size >> 24)
	length[1] = byte(size >> 16)
	length[2] = byte(size >> 8)
	length[3] = byte(size)

	// First 3 bytes should be 0 for small payloads
	if length[0] != 0 || length[1] != 0 || length[2] != 0 {
		t.Errorf("high bytes should be 0 for small payload, got %v", length[:3])
	}
	if length[3] != byte(size) {
		t.Errorf("low byte = %d, want %d", length[3], size)
	}
}

func TestAgentProtocolLengthPrefixLarge(t *testing.T) {
	// Large payload (> 256 bytes) to test multi-byte length
	size := 1024

	length := make([]byte, 4)
	length[0] = byte(size >> 24)
	length[1] = byte(size >> 16)
	length[2] = byte(size >> 8)
	length[3] = byte(size)

	decoded := int(length[0])<<24 | int(length[1])<<16 | int(length[2])<<8 | int(length[3])
	if decoded != size {
		t.Errorf("decoded = %d, want %d", decoded, size)
	}
}

func TestAgentProtocolLengthPrefixMax(t *testing.T) {
	// Maximum 4-byte value
	size := 16 * 1024 * 1024 // 16MB

	length := make([]byte, 4)
	length[0] = byte(size >> 24)
	length[1] = byte(size >> 16)
	length[2] = byte(size >> 8)
	length[3] = byte(size)

	decoded := int(length[0])<<24 | int(length[1])<<16 | int(length[2])<<8 | int(length[3])
	if decoded != size {
		t.Errorf("decoded = %d, want %d", decoded, size)
	}
}

// === Agent protocol: response parsing ===

func TestAgentProtocolResponseParsing(t *testing.T) {
	resp := struct {
		Data     []byte `json:"data"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	}{
		Data:     []byte("hello world\n"),
		ExitCode: 0,
	}

	data, _ := json.Marshal(resp)
	var decoded struct {
		Data     []byte `json:"data"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	}
	json.Unmarshal(data, &decoded)

	if string(decoded.Data) != "hello world\n" {
		t.Errorf("Data = %q", decoded.Data)
	}
	if decoded.ExitCode != 0 {
		t.Errorf("ExitCode = %d", decoded.ExitCode)
	}
	if decoded.Error != "" {
		t.Errorf("Error = %q", decoded.Error)
	}
}

func TestAgentProtocolResponseWithError(t *testing.T) {
	resp := struct {
		Data     []byte `json:"data"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	}{
		ExitCode: 127,
		Error:    "command not found",
	}

	data, _ := json.Marshal(resp)
	var decoded struct {
		Data     []byte `json:"data"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	}
	json.Unmarshal(data, &decoded)

	if decoded.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127", decoded.ExitCode)
	}
	if decoded.Error != "command not found" {
		t.Errorf("Error = %q", decoded.Error)
	}
}

// === readFullConn ===

func TestReadFullConnComplete(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		server.Write([]byte("test data"))
	}()

	buf := make([]byte, 9)
	n, err := readFullConn(client, buf)
	if err != nil {
		t.Fatalf("readFullConn: %v", err)
	}
	if n != 9 {
		t.Errorf("n = %d, want 9", n)
	}
	if string(buf) != "test data" {
		t.Errorf("buf = %q, want 'test data'", string(buf))
	}
}

func TestReadFullConnPartialReads(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Write in small chunks
	go func() {
		server.Write([]byte("ab"))
		time.Sleep(5 * time.Millisecond)
		server.Write([]byte("cd"))
		time.Sleep(5 * time.Millisecond)
		server.Write([]byte("ef"))
	}()

	buf := make([]byte, 6)
	n, err := readFullConn(client, buf)
	if err != nil {
		t.Fatalf("readFullConn: %v", err)
	}
	if n != 6 {
		t.Errorf("n = %d, want 6", n)
	}
	if string(buf) != "abcdef" {
		t.Errorf("buf = %q, want 'abcdef'", string(buf))
	}
}

func TestReadFullConnEOFBeforeFull(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	go func() {
		server.Write([]byte("short"))
		server.Close()
	}()

	buf := make([]byte, 20)
	_, err := readFullConn(client, buf)
	if err == nil {
		t.Error("should error when connection closes before buffer is full")
	}
}

func TestReadFullConnEmptyBuffer(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	buf := make([]byte, 0)
	n, err := readFullConn(client, buf)
	if err != nil {
		t.Fatalf("readFullConn empty: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

// === Base64 command encoding (used by exec-direct) ===

func TestBase64CommandEncoding(t *testing.T) {
	tests := []struct {
		command string
	}{
		{"echo hello"},
		{"ls -la /"},
		{"cat /etc/passwd"},
		{"echo 'hello world'"},
		{"sh -c \"echo hi && echo bye\""},
		{"export FOO=bar; echo $FOO"},
	}

	for _, tt := range tests {
		encoded := base64.StdEncoding.EncodeToString([]byte(tt.command))
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("decode %q: %v", encoded, err)
		}
		if string(decoded) != tt.command {
			t.Errorf("roundtrip failed: got %q, want %q", decoded, tt.command)
		}
	}
}

func TestBase64CommandWithSpecialChars(t *testing.T) {
	// Commands with characters that need careful handling
	commands := []string{
		"echo 'it\\'s'",
		"echo \"hello\\nworld\"",
		"find . -name '*.go' -exec wc -l {} \\;",
		"echo $(whoami)",
		"echo `date`",
	}

	for _, cmd := range commands {
		encoded := base64.StdEncoding.EncodeToString([]byte(cmd))
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("decode failed for %q: %v", cmd, err)
		}
		if string(decoded) != cmd {
			t.Errorf("roundtrip: got %q, want %q", decoded, cmd)
		}
	}
}

// === Agent request JSON structure ===

func TestAgentRequestJSONStructure(t *testing.T) {
	// The agent protocol expects this exact structure
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"type": "exec",
		"id":   "e",
		"exec": map[string]string{"command": "echo test"},
	})

	var parsed map[string]interface{}
	json.Unmarshal(reqJSON, &parsed)

	if parsed["type"] != "exec" {
		t.Errorf("type = %v, want exec", parsed["type"])
	}
	if parsed["id"] != "e" {
		t.Errorf("id = %v, want e", parsed["id"])
	}
	exec := parsed["exec"].(map[string]interface{})
	if exec["command"] != "echo test" {
		t.Errorf("command = %v, want 'echo test'", exec["command"])
	}
}
