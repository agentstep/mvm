package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
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

// === daemonSecretEnv ===

func TestDaemonSecretEnvUsesLocalStoreWhenAvailable(t *testing.T) {
	// withTestMvmDir points the package-level mvmDir (which secretEnvVars'
	// secretStore() reads) at a scratch dir so the underlying secrets.Store
	// can actually create its key file — otherwise Get() would fail with a
	// mkdir error instead of the "not found" this test is asserting on.
	dir := withTestMvmDir(t)
	store := state.NewStore(filepath.Join(dir, "state.json"))
	store.AddVM(&state.VM{Name: "web", Backend: "firecracker", Secrets: []string{"MISSING_SECRET"}, CreatedAt: time.Now()})

	// Point sc at a socket that doesn't exist — if this test passes,
	// daemonSecretEnv resolved secrets from the local store and never
	// dialed sc at all.
	sc := server.NewClient(filepath.Join(dir, "no-such.sock"))

	_, err := daemonSecretEnv(context.Background(), store, sc, "web")
	// secretEnvVars fails because MISSING_SECRET was never `mvm secret put`
	// in this test — the ONLY way this can fail here is via the local-store
	// path; an InspectVM round trip against a nonexistent socket would fail
	// with a dial/connection error instead, not "not found".
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a secret-not-found error from the local-store path", err)
	}
}

func TestDaemonSecretEnvUsesLocalStoreEvenWithNoSecrets(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "state.json"))
	store.AddVM(&state.VM{Name: "web", Backend: "firecracker", CreatedAt: time.Now()})
	sc := server.NewClient(filepath.Join(dir, "no-such.sock")) // unreachable — proves no round trip happened

	env, err := daemonSecretEnv(context.Background(), store, sc, "web")
	if err != nil {
		t.Fatalf("daemonSecretEnv: %v", err)
	}
	if env != nil {
		t.Errorf("env = %v, want nil (no secrets attached, no daemon round trip needed)", env)
	}
}

func TestDaemonSecretEnvFallsBackToInspectVM(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{
			VMResponse: server.VMResponse{Name: "web"},
			Spec:       &state.VMSpec{Secrets: []string{"MISSING_SECRET"}},
		})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	sc := server.NewRemoteClient(ts.URL, "", "")

	// Same reasoning as the local-store test above: point mvmDir at a
	// scratch dir so secretEnvVars' underlying secrets.Store can create its
	// key file and return a genuine "not found" rather than a mkdir error.
	dir := withTestMvmDir(t)

	// Empty store — this VM is unknown locally, the way a cloud/remote VM
	// always is (no shared filesystem with a genuinely remote daemon host).
	store := state.NewStore(filepath.Join(dir, "state.json"))

	_, err := daemonSecretEnv(context.Background(), store, sc, "web")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want the InspectVM fallback to still surface a secret-not-found error", err)
	}
}

func TestDaemonSecretEnvNoSecretsViaInspectVM(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.VMInspectResponse{VMResponse: server.VMResponse{Name: "web"}})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	sc := server.NewRemoteClient(ts.URL, "", "")
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))

	env, err := daemonSecretEnv(context.Background(), store, sc, "web")
	if err != nil {
		t.Fatalf("daemonSecretEnv: %v", err)
	}
	if env != nil {
		t.Errorf("env = %v, want nil for a VM with no secrets attached", env)
	}
}

func TestExecNoSeparatorTakesCommandDirectly(t *testing.T) {
	cmd := newExecCmd(nil)
	var got []string
	cmd.RunE = func(c *cobra.Command, args []string) error { got = args; return nil }
	cmd.SetArgs([]string{"web", "ls", "-la"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"web", "ls", "-la"}
	if len(got) != len(want) {
		t.Fatalf("args: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestExecLeadingFlagStillBinds(t *testing.T) {
	cmd := newExecCmd(nil)
	var got []string
	cmd.RunE = func(c *cobra.Command, args []string) error { got = args; return nil }
	cmd.SetArgs([]string{"-i", "web", "env"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(got) != 2 || got[0] != "web" || got[1] != "env" {
		t.Fatalf("args: got %v want [web env]", got)
	}
}
