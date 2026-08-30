package server

import (
	"os"
	"strings"
	"testing"
)

// stream:true must actually stream.
//
// It used to call the blocking Exec, wait for the command to finish, and emit the whole buffer as
// one stdout frame plus an exit frame. The wire shape was right, so no client could tell — but
// nothing arrived until the command was over. Watching a build was impossible, and the TS
// provider's spawn() handed back a "stream" that was a finished string.
func TestStreamingExecDoesNotBufferTheWholeCommand(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "client.ExecStream(ctx, req.Command, req.Stdin") {
		t.Error("the streaming path does not call ExecStream — it is still waiting for the command to finish")
	}

	// The decisive ordering check: the stream branch must be taken BEFORE the blocking Exec runs.
	branch := strings.Index(body, "if req.Stream {")
	blocking := strings.Index(body, "client.Exec(ctx, req.Command, req.Stdin)")
	if branch == -1 || blocking == -1 {
		t.Fatal("expected both a stream branch and a buffered Exec in execAndRespond")
	}
	if branch > blocking {
		t.Error("execAndRespond calls the blocking Exec before checking req.Stream — every frame would arrive at once, after the command ended")
	}
}

// The guest has always sent stdout and stderr as separate frame types; the daemon discarded the
// split. Agent harnesses branch on stderr, so merging it is a loss of information, not a detail.
func TestStreamingExecKeepsStderrSeparate(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	if !strings.Contains(string(src), `"type": f.Kind`) {
		t.Error("stream frames do not carry the guest's own frame kind — stdout and stderr are being flattened")
	}
}

// A streaming response outlives the listener's WriteTimeout, which is armed once before the handler
// runs. Without a per-write deadline the write starts failing mid-command, over TCP only — the
// transport a containerised client is obliged to use.
func TestStreamingExecExtendsTheWriteDeadline(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "SetWriteDeadline") {
		t.Error("the streaming writer never extends its write deadline — a long command dies at the listener's WriteTimeout")
	}
	if !strings.Contains(body, "rc.Flush()") {
		t.Error("frames are not flushed, so they buffer and arrive together — which is the bug this replaced")
	}
}

// A failure after the header is sent cannot become a status code. Silence would read to the client
// as a clean exit, which is the one interpretation that loses data without looking like it.
func TestStreamingExecReportsMidStreamFailure(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	if !strings.Contains(string(src), `"type": "error"`) {
		t.Error("a mid-stream failure emits no error frame — the client cannot distinguish it from the command ending")
	}
}

// The hijacked interactive session must clear the deadlines net/http armed for a request/response
// exchange. Over TCP those are ReadTimeout 30s / WriteTimeout 5m, and Hijack does not clear them —
// so interactive exec against a REMOTE daemon has been dying about 30 seconds in.
func TestHijackClearsConnectionDeadlines(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	body := string(src)
	hijack := strings.Index(body, "hj.Hijack()")
	clear := strings.Index(body, "conn.SetDeadline(time.Time{})")
	if hijack == -1 {
		t.Fatal("no hijack found")
	}
	if clear == -1 {
		t.Fatal("the hijacked connection never clears its deadline — a remote interactive session dies at the listener's ReadTimeout")
	}
	if clear < hijack {
		t.Error("the deadline is cleared before the hijack, where it has nothing to act on")
	}
}
