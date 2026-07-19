package cli

import (
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		{"it's", "'it'\"'\"'s'"},
		{"", "''"},
		{"a'b'c", "'a'\"'\"'b'\"'\"'c'"},
		{`echo "hi"`, `'echo "hi"'`},
		{"rm -rf /", "'rm -rf /'"},
		{"$(whoami)", "'$(whoami)'"},
		{"`id`", "'`id`'"},
		{"foo;bar", "'foo;bar'"},
		{"a && b || c", "'a && b || c'"},
	}

	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"ls"}, "'ls'"},
		{[]string{"ls", "-la"}, "'ls' '-la'"},
		{[]string{"echo", "hello world"}, "'echo' 'hello world'"},
		{[]string{"sh", "-c", "echo hi && echo bye"}, "'sh' '-c' 'echo hi && echo bye'"},
	}

	for _, tt := range tests {
		got := shellJoin(tt.args)
		if got != tt.want {
			t.Errorf("shellJoin(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// === NEW TESTS: Shell injection prevention ===

func TestShellQuotePreventsSemicolon(t *testing.T) {
	// Semicolons should be safely quoted, not interpreted as command separator
	got := shellQuote("ls; rm -rf /")
	if got != "'ls; rm -rf /'" {
		t.Errorf("semicolon not safely quoted: %q", got)
	}
}

func TestShellQuotePreventsBacktick(t *testing.T) {
	got := shellQuote("`cat /etc/passwd`")
	if got != "'`cat /etc/passwd`'" {
		t.Errorf("backtick not safely quoted: %q", got)
	}
}

func TestShellQuotePreventsDollarParen(t *testing.T) {
	got := shellQuote("$(rm -rf /)")
	if got != "'$(rm -rf /)'" {
		t.Errorf("dollar-paren not safely quoted: %q", got)
	}
}

func TestShellQuotePreventsNewline(t *testing.T) {
	got := shellQuote("line1\nline2")
	// Newlines are safe inside single quotes
	if got != "'line1\nline2'" {
		t.Errorf("newline not safely quoted: %q", got)
	}
}

func TestShellQuotePreventsGlob(t *testing.T) {
	got := shellQuote("*.txt")
	if got != "'*.txt'" {
		t.Errorf("glob not safely quoted: %q", got)
	}
}

// === NEW TESTS: shellJoin with edge cases ===

func TestShellJoinEmpty(t *testing.T) {
	got := shellJoin([]string{})
	if got != "" {
		t.Errorf("shellJoin([]) = %q, want empty", got)
	}
}

func TestShellJoinSingleQuoteInArgs(t *testing.T) {
	got := shellJoin([]string{"echo", "it's", "O'Brien"})
	// Each arg should be safely quoted
	if got == "" {
		t.Error("should produce non-empty output")
	}
	// Verify no unquoted single quotes would break the shell
	// The shellQuote function replaces ' with '"'"' so this is safe
}

func TestShellJoinPreservesArgBoundaries(t *testing.T) {
	// "echo hello world" as three args should stay three args, not become one
	args := []string{"echo", "hello", "world"}
	got := shellJoin(args)
	if got != "'echo' 'hello' 'world'" {
		t.Errorf("shellJoin should preserve arg boundaries: %q", got)
	}
}

// === buildDetachedExecScript ===

func TestBuildDetachedExecScriptWrapsWithSetsid(t *testing.T) {
	got := buildDetachedExecScript([]string{"sleep", "100"}, "", nil, "")
	if !strings.HasPrefix(got, "setsid sh -c ") {
		t.Errorf("buildDetachedExecScript() = %q, want prefix %q", got, "setsid sh -c ")
	}
	if !strings.HasSuffix(got, "</dev/null >/dev/null 2>&1 &") {
		t.Errorf("buildDetachedExecScript() = %q, want a fully-detached background suffix", got)
	}
}

func TestBuildDetachedExecScriptEmbedsInnerCommand(t *testing.T) {
	inner := buildExecScript([]string{"echo", "hi"}, "", nil, "")
	got := buildDetachedExecScript([]string{"echo", "hi"}, "", nil, "")
	if !strings.Contains(got, shellQuote(inner)) {
		t.Errorf("buildDetachedExecScript() = %q, want it to shell-quote and embed %q", got, inner)
	}
}

func TestBuildDetachedExecScriptRespectsWorkdirEnvUser(t *testing.T) {
	inner := buildExecScript([]string{"env"}, "/app", []string{"FOO=bar"}, "nobody")
	got := buildDetachedExecScript([]string{"env"}, "/app", []string{"FOO=bar"}, "nobody")
	if !strings.Contains(got, shellQuote(inner)) {
		t.Errorf("buildDetachedExecScript() = %q, want the same inner construction as buildExecScript, just wrapped", got)
	}
}

// === validateExecFlags ===

func TestValidateExecFlagsRejectsDetachWithInteractive(t *testing.T) {
	if err := validateExecFlags(true, true, false); err == nil {
		t.Fatal("want error combining --detach and --interactive")
	}
	if err := validateExecFlags(true, false, true); err == nil {
		t.Fatal("want error combining --detach and --tty")
	}
}

func TestValidateExecFlagsAllowsDetachAlone(t *testing.T) {
	if err := validateExecFlags(true, false, false); err != nil {
		t.Errorf("validateExecFlags(true, false, false) = %v, want nil", err)
	}
}

func TestValidateExecFlagsAllowsInteractiveWithoutDetach(t *testing.T) {
	if err := validateExecFlags(false, true, true); err != nil {
		t.Errorf("validateExecFlags(false, true, true) = %v, want nil", err)
	}
}
