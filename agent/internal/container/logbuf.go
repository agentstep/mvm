package container

import (
	"strings"
	"sync"
	"time"
)

// maxLogBytes caps one service's retained output.
//
// A ring buffer rather than a file: a service that logs in a loop would
// otherwise fill the guest disk, and the rootfs is the VM's durable state, so
// running it out of space breaks far more than logging. Recent output is what
// anyone actually reads; older output is dropped rather than persisted.
const maxLogBytes = 256 * 1024

// LogLine is one captured line of service output.
type LogLine struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"` // "stdout" or "stderr"
	Text   string    `json:"text"`
}

// logBuffer retains the most recent output of one service, bounded by total
// bytes rather than line count so a single enormous line cannot blow the cap.
type logBuffer struct {
	mu    sync.Mutex
	lines []LogLine
	bytes int
}

func newLogBuffer() *logBuffer { return &logBuffer{} }

// Append records output, splitting on newlines and dropping the oldest lines
// once the cap is exceeded.
func (b *logBuffer) Append(stream string, data []byte) {
	if len(data) == 0 {
		return
	}
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, text := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		// A single line larger than the whole cap would evict everything and
		// still not fit, so truncate it instead.
		if len(text) > maxLogBytes {
			text = text[:maxLogBytes]
		}
		b.lines = append(b.lines, LogLine{At: now, Stream: stream, Text: text})
		b.bytes += len(text)
	}

	for b.bytes > maxLogBytes && len(b.lines) > 0 {
		b.bytes -= len(b.lines[0].Text)
		b.lines = b.lines[1:]
	}
}

// Tail returns the most recent n lines, or all of them when n <= 0.
func (b *logBuffer) Tail(n int) []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()

	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]LogLine, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// Len reports the retained line count. Test helper.
func (b *logBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}

// Bytes reports the retained byte count. Test helper.
func (b *logBuffer) Bytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes
}
