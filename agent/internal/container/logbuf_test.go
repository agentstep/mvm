package container

import (
	"strings"
	"sync"
	"testing"
)

func TestLogBufferSplitsLines(t *testing.T) {
	b := newLogBuffer()
	b.Append("stdout", []byte("one\ntwo\nthree\n"))
	if got := b.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	lines := b.Tail(0)
	if lines[0].Text != "one" || lines[2].Text != "three" {
		t.Errorf("lines = %+v, want one/two/three", lines)
	}
	if lines[0].Stream != "stdout" {
		t.Errorf("stream = %q, want stdout", lines[0].Stream)
	}
}

func TestLogBufferTail(t *testing.T) {
	b := newLogBuffer()
	b.Append("stdout", []byte("a\nb\nc\nd\n"))
	got := b.Tail(2)
	if len(got) != 2 || got[0].Text != "c" || got[1].Text != "d" {
		t.Errorf("Tail(2) = %+v, want c/d", got)
	}
	if n := len(b.Tail(100)); n != 4 {
		t.Errorf("Tail(100) = %d lines, want all 4", n)
	}
}

// TestLogBufferIsBounded is the point of the ring buffer. A service that logs
// in a loop would otherwise grow without limit, and since the rootfs is the
// VM's durable state, filling it breaks much more than logging.
func TestLogBufferIsBounded(t *testing.T) {
	b := newLogBuffer()
	line := strings.Repeat("x", 1024)
	for i := 0; i < 2000; i++ { // ~2MB written into a 256KB cap
		b.Append("stdout", []byte(line+"\n"))
	}
	if got := b.Bytes(); got > maxLogBytes {
		t.Errorf("retained %d bytes, cap is %d", got, maxLogBytes)
	}
	// The most recent output must survive; the oldest is what gets dropped.
	if b.Len() == 0 {
		t.Fatal("buffer dropped everything")
	}
}

// TestLogBufferSingleOversizedLine — one line larger than the entire cap would
// evict everything and still not fit, so it must be truncated instead of
// looping forever or blowing the bound.
func TestLogBufferSingleOversizedLine(t *testing.T) {
	b := newLogBuffer()
	b.Append("stdout", []byte(strings.Repeat("y", maxLogBytes*3)))
	if got := b.Bytes(); got > maxLogBytes {
		t.Errorf("retained %d bytes for one oversized line, cap is %d", got, maxLogBytes)
	}
	if b.Len() != 1 {
		t.Errorf("Len = %d, want the truncated line retained", b.Len())
	}
}

func TestLogBufferEmptyAppendIsNoop(t *testing.T) {
	b := newLogBuffer()
	b.Append("stdout", nil)
	b.Append("stdout", []byte(""))
	if b.Len() != 0 {
		t.Errorf("Len = %d, want 0", b.Len())
	}
}

// Output arrives from two goroutines (stdout and stderr) while a reader may be
// tailing. Run with -race.
func TestLogBufferConcurrent(t *testing.T) {
	b := newLogBuffer()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Append("stdout", []byte("line\n"))
				b.Tail(10)
			}
		}()
	}
	wg.Wait()
	if b.Bytes() > maxLogBytes {
		t.Errorf("retained %d bytes under concurrency, cap is %d", b.Bytes(), maxLogBytes)
	}
}
