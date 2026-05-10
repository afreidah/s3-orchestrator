// -------------------------------------------------------------------------------
// Cache Tee Body - Stream-Time Cache Population
//
// Author: Alex Freidah
//
// cacheTeeBody wraps an io.ReadCloser, copying bytes into a fixed-capacity
// buffer as they flow to the client. On a clean read - EOF reached after
// exactly the expected number of bytes - the buffer is handed to a cache
// finalizer. On any divergence (early Close, mid-stream error, short read,
// or backend overshooting its announced Content-Length) the buffer is
// dropped without invoking the finalizer, so partial or inconsistent
// objects never enter the cache.
//
// This is what makes the read-side cache safe for arbitrary object sizes:
// the proxy never holds more than expectedSize bytes in heap, and that
// only when the cache has already admitted the object. Oversized objects
// stream straight through with no buffering at all.
// -------------------------------------------------------------------------------

package proxy

import (
	"errors"
	"io"
)

// errCacheSinkOverflow is returned by cacheSink.Write when the caller
// tries to push past the pre-sized capacity. Surfaced to cacheTeeBody so
// it can mark the entry abandoned and stop teeing without aborting the
// underlying read.
var errCacheSinkOverflow = errors.New("cache tee: backend returned more bytes than announced size")

// cacheSink is a write-only buffer with a fixed capacity. Writes past
// the cap return errCacheSinkOverflow and the sink keeps the bytes it
// already accumulated for inspection by the tee.
type cacheSink struct {
	buf []byte
	cap int
}

// newCacheSink allocates a sink that can hold up to cap bytes. The
// backing slice is pre-allocated at full capacity to avoid append-time
// growth amortization while the response is streaming.
func newCacheSink(c int) *cacheSink {
	return &cacheSink{buf: make([]byte, 0, c), cap: c}
}

// Write appends p to the sink. Returns errCacheSinkOverflow when the
// addition would push past cap; in that case nothing is appended.
func (s *cacheSink) Write(p []byte) (int, error) {
	if len(s.buf)+len(p) > s.cap {
		return 0, errCacheSinkOverflow
	}
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// bytes returns the accumulated buffer. Callers must not mutate the
// returned slice; ownership transfers when the tee invokes its
// finalizer at clean EOF.
func (s *cacheSink) bytes() []byte { return s.buf }

// cacheTeeBody wraps an io.ReadCloser and copies bytes into a fixed-
// capacity sink as they flow to the consumer. Calls onComplete with the
// accumulated bytes exactly once - and only if - the read reached EOF
// after exactly expected bytes were observed. Any other termination
// (Close before EOF, mid-stream error, short read, sink overflow) drops
// the buffer silently and onComplete is never invoked.
type cacheTeeBody struct {
	io.ReadCloser
	sink       *cacheSink
	expected   int64
	bytesRead  int64
	onComplete func(data []byte)
	abandoned  bool
	finalized  bool
}

// newCacheTeeBody constructs a teeing wrapper sized to expected bytes.
// onComplete is invoked at most once, after a clean EOF read of exactly
// expected bytes; the byte slice handed to it is owned by the callback
// and freed by the caller.
func newCacheTeeBody(body io.ReadCloser, expected int64, onComplete func(data []byte)) *cacheTeeBody {
	return &cacheTeeBody{
		ReadCloser: body,
		sink:       newCacheSink(int(expected)),
		expected:   expected,
		onComplete: onComplete,
	}
}

// Read forwards to the underlying body and copies returned bytes into
// the sink. Surfaces sink overflow as "abandoned" without propagating
// the error to the consumer (the consumer's stream is the source of
// truth; cache population is best-effort). Finalizes on clean EOF.
func (t *cacheTeeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 && !t.abandoned {
		if _, werr := t.sink.Write(p[:n]); werr != nil {
			// Backend overshooting Content-Length - abandon caching but
			// keep streaming to the consumer.
			t.abandoned = true
			t.sink.buf = nil
		} else {
			t.bytesRead += int64(n)
		}
	}
	if errors.Is(err, io.EOF) && !t.abandoned && !t.finalized && t.bytesRead == t.expected {
		t.onComplete(t.sink.bytes())
		t.finalized = true
	}
	return n, err
}

// Close forwards to the underlying body. If Close arrives before EOF
// the read is considered abandoned and the sink contents are dropped.
func (t *cacheTeeBody) Close() error {
	if !t.finalized {
		t.abandoned = true
		t.sink.buf = nil
	}
	return t.ReadCloser.Close()
}
