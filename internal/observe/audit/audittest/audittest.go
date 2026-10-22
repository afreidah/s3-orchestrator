// -------------------------------------------------------------------------------
// Audit Test Helpers - Capturing Emitted Audit Entries
//
// Author: Alex Freidah
//
// Audit entries are written through the default slog logger, so a test that
// asserts on them swaps in a JSON handler, runs the code under test, and reads
// back the entries marked as audit. Tests that call Capture must not run in
// parallel with other tests that log, since the default logger is global.
// -------------------------------------------------------------------------------

package audittest

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Entry is one decoded audit log line.
type Entry map[string]any

// Capture installs a JSON handler for the duration of body and returns every
// audit entry body emitted, in order.
func Capture(t testing.TB, body func()) []Entry {
	t.Helper()
	var buf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	body()

	var out []Entry
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var e Entry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e["audit"] == true {
			out = append(out, e)
		}
	}
	return out
}

// Named returns the entries whose event is name.
func Named(entries []Entry, name string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

// WithPrefix returns the entries whose event starts with prefix.
func WithPrefix(entries []Entry, prefix string) []Entry {
	var out []Entry
	for _, e := range entries {
		if ev, _ := e["event"].(string); strings.HasPrefix(ev, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// lockedBuffer is a bytes.Buffer safe for the concurrent writes of code that
// logs from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p under the lock.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the buffered text under the lock.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
