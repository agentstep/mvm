package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDockerfile_BasicRUN(t *testing.T) {
	content := `FROM debian:bookworm
RUN apt-get update
RUN apt-get install -y curl
`
	steps := parseDockerfileFromString(t, content)

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].Directive != "RUN" || steps[0].Args != "apt-get update" {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1].Directive != "RUN" || steps[1].Args != "apt-get install -y curl" {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

func TestParseDockerfile_ENV(t *testing.T) {
	content := `FROM debian:bookworm
ENV LANG=C.UTF-8
ENV MY_VAR hello world
`
	steps := parseDockerfileFromString(t, content)

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].Directive != "ENV" || steps[0].Args != "LANG=C.UTF-8" {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1].Directive != "ENV" || steps[1].Args != "MY_VAR hello world" {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

func TestParseDockerfile_LineContinuation(t *testing.T) {
	content := `FROM debian:bookworm
RUN apt-get update && \
    apt-get install -y \
    curl \
    wget
`
	steps := parseDockerfileFromString(t, content)

	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
	if steps[0].Directive != "RUN" {
		t.Errorf("expected RUN directive, got %s", steps[0].Directive)
	}
	// The continuation should join lines with spaces.
	expected := "apt-get update &&  apt-get install -y  curl  wget"
	if steps[0].Args != expected {
		t.Errorf("expected args %q, got %q", expected, steps[0].Args)
	}
}

func TestParseDockerfile_CommentsAndEmptyLines(t *testing.T) {
	content := `# This is a comment
FROM debian:bookworm

# Another comment
RUN echo hello

`
	steps := parseDockerfileFromString(t, content)

	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
	if steps[0].Args != "echo hello" {
		t.Errorf("expected 'echo hello', got %q", steps[0].Args)
	}
}

func TestParseDockerfile_COPYWarning(t *testing.T) {
	content := `FROM debian:bookworm
COPY . /app
RUN echo done
`
	// Redirect stderr to capture the warning.
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	steps := parseDockerfileFromString(t, content)

	w.Close()
	os.Stderr = oldStderr

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	r.Close()
	warning := string(buf[:n])

	if len(steps) != 1 {
		t.Fatalf("expected 1 step (COPY skipped), got %d", len(steps))
	}
	if steps[0].Directive != "RUN" {
		t.Errorf("expected RUN, got %s", steps[0].Directive)
	}
	if warning == "" {
		t.Error("expected COPY warning on stderr, got nothing")
	}
}

func TestParseDockerfile_SkipsOtherDirectives(t *testing.T) {
	content := `FROM debian:bookworm
WORKDIR /app
EXPOSE 8080
CMD ["echo", "hello"]
RUN echo kept
`
	steps := parseDockerfileFromString(t, content)

	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
	if steps[0].Directive != "RUN" || steps[0].Args != "echo kept" {
		t.Errorf("unexpected step: %+v", steps[0])
	}
}

func TestParseDockerfile_FileNotFound(t *testing.T) {
	_, err := ParseDockerfile("/nonexistent/Dockerfile")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseEnvArg(t *testing.T) {
	tests := []struct {
		input string
		key   string
		value string
	}{
		{"LANG=C.UTF-8", "LANG", "C.UTF-8"},
		{"MY_VAR hello world", "MY_VAR", "hello world"},
		{"EMPTY=", "EMPTY", ""},
		{"SOLO", "SOLO", ""},
		{"", "", ""},
	}

	for _, tt := range tests {
		k, v := parseEnvArg(tt.input)
		if k != tt.key || v != tt.value {
			t.Errorf("parseEnvArg(%q) = (%q, %q), want (%q, %q)", tt.input, k, v, tt.key, tt.value)
		}
	}
}

func TestSplitDirective(t *testing.T) {
	tests := []struct {
		input     string
		directive string
		args      string
	}{
		{"RUN apt-get update", "RUN", "apt-get update"},
		{"FROM debian:bookworm", "FROM", "debian:bookworm"},
		{"ENV LANG=C.UTF-8", "ENV", "LANG=C.UTF-8"},
		{"run lowercase", "RUN", "lowercase"},
	}

	for _, tt := range tests {
		d, a := splitDirective(tt.input)
		if d != tt.directive || a != tt.args {
			t.Errorf("splitDirective(%q) = (%q, %q), want (%q, %q)", tt.input, d, a, tt.directive, tt.args)
		}
	}
}

// parseDockerfileFromString is a test helper that writes content to a temp file
// and calls ParseDockerfile.
func parseDockerfileFromString(t *testing.T, content string) []BuildStep {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp Dockerfile: %v", err)
	}
	steps, err := ParseDockerfile(path)
	if err != nil {
		t.Fatalf("ParseDockerfile: %v", err)
	}
	return steps
}
