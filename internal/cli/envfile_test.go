package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// === parseEnvFile ===

func TestParseEnvFileSkipsBlankAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	content := "FOO=bar\n\n# a comment\nBAZ=qux\n  \n#another\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	want := []string{"FOO=bar", "BAZ=qux"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parseEnvFile() = %v, want %v", got, want)
	}
}

func TestParseEnvFileRejectsBareKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("FOO=bar\nNOTKEYVALUE\n"), 0o644)
	if _, err := parseEnvFile(path); err == nil {
		t.Fatal("parseEnvFile() = nil error, want error for a bare-key line")
	}
}

func TestParseEnvFileMissingFile(t *testing.T) {
	if _, err := parseEnvFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("parseEnvFile() = nil error, want error for a missing file")
	}
}

func TestParseEnvFilePreservesEqualsInValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("URL=https://example.com/?a=b\n"), 0o644)
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	if len(got) != 1 || got[0] != "URL=https://example.com/?a=b" {
		t.Errorf("parseEnvFile() = %v, want [URL=https://example.com/?a=b]", got)
	}
}

// === mergeEnvFile ===

func TestMergeEnvFileFileFirstExplicitWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("FOO=fromfile\nBAR=fromfile\n"), 0o644)
	got, err := mergeEnvFile(path, []string{"FOO=fromflag"})
	if err != nil {
		t.Fatalf("mergeEnvFile: %v", err)
	}
	want := []string{"FOO=fromfile", "BAR=fromfile", "FOO=fromflag"}
	if len(got) != len(want) {
		t.Fatalf("mergeEnvFile() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mergeEnvFile()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMergeEnvFileEmptyPathPassthrough(t *testing.T) {
	got, err := mergeEnvFile("", []string{"A=b"})
	if err != nil {
		t.Fatalf("mergeEnvFile: %v", err)
	}
	if len(got) != 1 || got[0] != "A=b" {
		t.Errorf("mergeEnvFile(\"\", ...) = %v, want unchanged passthrough", got)
	}
}

func TestMergeEnvFilePropagatesParseError(t *testing.T) {
	if _, err := mergeEnvFile(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("mergeEnvFile() = nil error, want the missing-file error propagated")
	}
}
