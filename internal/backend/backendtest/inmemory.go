// -------------------------------------------------------------------------------
// Backendtest - In-Memory Object Backend
//
// Author: Alex Freidah
//
// A stateful stand-in for a real object backend: it stores what is put and
// returns it on get, so a round-trip test can assert on bytes rather than on
// a call sequence. That is what the generated MockObjectBackend cannot do
// without an expectation per call, and why this exists alongside it - reach
// for the generated mock when the assertion is "which calls happened", and
// for this when the assertion is "what ended up stored".
//
// Every failure mode a test needs to drive is an injectable field rather than
// a subclass: put/get/head/delete errors, a mid-stream read failure, a panic,
// a delete delay, and native-copy support that is off by default.
// -------------------------------------------------------------------------------

package backendtest

//go:generate mockgen -destination=mock_backend.go -package=backendtest github.com/afreidah/s3-orchestrator/internal/backend ObjectBackend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
)

// notFoundStatus is the HTTP status an absent key reports, so callers reaching
// for backend.IsNotFound classify it the way they would a real S3 404.
const notFoundStatus = 404

// Object is one stored object: the payload plus the metadata a caller can read
// back through Get or Head.
type Object struct {
	Data         []byte
	ContentType  string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
}

// InMemory is an in-memory backend.ObjectBackend. The zero value is not
// usable; construct with NewInMemory. Safe for concurrent use.
type InMemory struct {
	mu sync.Mutex

	// Objects is the backing store. Held under the same mutex as everything
	// else, so read it through Has/Snapshot rather than directly when the
	// system under test may still be running.
	Objects map[string]Object

	// Injected failures. Each is checked by the matching method and returned
	// in place of its normal result.
	PutErr      error
	GetErr      error
	HeadErr     error
	DeleteErr   error
	GetReadErr  error // surfaces from the body reader, not from GetObject itself
	GetPanic    bool  // GetObject panics instead of returning
	DeleteDelay time.Duration

	// Observations recorded on the way through, for tests asserting on how
	// the caller invoked the backend rather than on what it stored.
	LastPutBodySeekable   bool
	LastDeleteHadDeadline bool
	LastDeleteDeadline    time.Time

	// Native-copy support. CopyObject always exists so InMemory satisfies
	// backend.Copier, but it reports ErrCopyNotSupported until a test
	// sets CopyEnabled, leaving callers on the materialized-copy path.
	CopyEnabled bool
	CopyCalls   int
	CopyErr     error
}

// NewInMemory returns an empty in-memory backend.
func NewInMemory() *InMemory {
	return &InMemory{Objects: make(map[string]Object)}
}

var _ backend.ObjectBackend = (*InMemory)(nil)

// notFoundError is a status-code-bearing error, so backend.IsNotFound
// classifies an absent key the same way it classifies a real 404.
type notFoundError struct{ key string }

// Error implements error.
func (e *notFoundError) Error() string { return fmt.Sprintf("object %q not found", e.key) }

// HTTPStatusCode satisfies the interface backend.IsNotFound checks for.
func (e *notFoundError) HTTPStatusCode() int { return notFoundStatus }

// errReader fails every read with a fixed error, standing in for a body that
// dies partway through transfer.
type errReader struct{ err error }

// Read implements io.Reader.
func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// PutObject stores the body under key and returns an etag derived from its
// length. Records whether the body was seekable before reading it.
func (m *InMemory) PutObject(
	_ context.Context, key string, body io.Reader, _ int64, contentType string, metadata map[string]string,
) (string, error) {
	_, seekable := body.(io.Seeker)

	m.mu.Lock()
	err := m.PutErr
	m.LastPutBodySeekable = seekable
	m.mu.Unlock()
	if err != nil {
		return "", err
	}

	// Read outside the lock: a pipe-based copy would otherwise deadlock
	// against the writer, which needs the same lock to finish.
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	etag := fmt.Sprintf(`"%x"`, len(data))

	m.mu.Lock()
	m.Objects[key] = Object{Data: data, ContentType: contentType, ETag: etag, Metadata: metadata}
	m.mu.Unlock()
	return etag, nil
}

// parseByteRange resolves an RFC 9110 single byte-range against an object of
// size total, returning the inclusive offsets. ok is false when the header is
// absent or not a form this fake supports, in which case the caller serves the
// whole object - the same fallback a real backend applies to a range it cannot
// satisfy in the "ignore unsatisfiable" direction.
func parseByteRange(header string, total int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	from, to, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}

	switch {
	case from == "": // suffix form: last N bytes
		n, err := strconv.ParseInt(to, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(total-n, 0), total - 1, true
	case to == "": // open-ended: from N to the end
		start, err := strconv.ParseInt(from, 10, 64)
		if err != nil || start < 0 || start >= total {
			return 0, 0, false
		}
		return start, total - 1, true
	}

	start, err := strconv.ParseInt(from, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err = strconv.ParseInt(to, 10, 64)
	if err != nil || start < 0 || start > end || start >= total {
		return 0, 0, false
	}
	return start, min(end, total-1), true
}

// GetObject returns the stored object, honouring a single byte-range when one
// is supplied. The body is a copy, so the caller can read it after the lock is
// released.
func (m *InMemory) GetObject(_ context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.GetPanic {
		panic("injected GetObject panic for testing")
	}
	if m.GetErr != nil {
		return nil, m.GetErr
	}
	obj, ok := m.Objects[key]
	if !ok {
		return nil, &notFoundError{key: key}
	}

	data := bytes.Clone(obj.Data)
	var contentRange string
	if start, end, ok := parseByteRange(rangeHeader, int64(len(data))); ok {
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))
		data = data[start : end+1]
	}

	body := io.NopCloser(bytes.NewReader(data))
	if m.GetReadErr != nil {
		body = io.NopCloser(&errReader{err: m.GetReadErr})
	}
	return &backend.GetObjectResult{
		Body:         body,
		Size:         int64(len(data)),
		ContentType:  obj.ContentType,
		ETag:         obj.ETag,
		LastModified: obj.LastModified,
		ContentRange: contentRange,
		Metadata:     obj.Metadata,
	}, nil
}

// HeadObject returns the stored object's metadata without its payload.
func (m *InMemory) HeadObject(_ context.Context, key string) (*backend.HeadObjectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.HeadErr != nil {
		return nil, m.HeadErr
	}
	obj, ok := m.Objects[key]
	if !ok {
		return nil, &notFoundError{key: key}
	}
	return &backend.HeadObjectResult{
		Size:         int64(len(obj.Data)),
		ContentType:  obj.ContentType,
		ETag:         obj.ETag,
		LastModified: obj.LastModified,
		Metadata:     obj.Metadata,
	}, nil
}

// DeleteObject removes key, recording whether the caller supplied a deadline
// so timeout-propagation tests can assert on it.
func (m *InMemory) DeleteObject(ctx context.Context, key string) error {
	m.mu.Lock()
	delay := m.DeleteDelay
	m.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		m.LastDeleteHadDeadline = true
		m.LastDeleteDeadline = dl
	} else {
		m.LastDeleteHadDeadline = false
	}
	if m.DeleteErr != nil {
		return m.DeleteErr
	}
	delete(m.Objects, key)
	return nil
}

// CopyObject satisfies backend.Copier. Reports ErrCopyNotSupported
// until CopyEnabled is set, so callers stay on the materialized-copy path
// unless a test opts into the native one.
func (m *InMemory) CopyObject(_ context.Context, srcKey, dstKey, _ string, _ map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.CopyEnabled {
		return "", backend.ErrCopyNotSupported
	}
	m.CopyCalls++
	if m.CopyErr != nil {
		return "", m.CopyErr
	}
	src, ok := m.Objects[srcKey]
	if !ok {
		return "", &notFoundError{key: srcKey}
	}
	m.Objects[dstKey] = src
	return src.ETag, nil
}

// Has reports whether key is stored. Takes the lock, so it is safe to call
// while the system under test is still running.
func (m *InMemory) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.Objects[key]
	return ok
}

// SetPutErr swaps the injected PutObject failure under the lock, for a test
// that changes the backend's behaviour while it is already in use. Fields set
// during setup, before anything calls the backend, can be assigned directly.
func (m *InMemory) SetPutErr(err error) { m.set(func() { m.PutErr = err }) }

// SetGetErr swaps the injected GetObject failure under the lock.
func (m *InMemory) SetGetErr(err error) { m.set(func() { m.GetErr = err }) }

// SetHeadErr swaps the injected HeadObject failure under the lock.
func (m *InMemory) SetHeadErr(err error) { m.set(func() { m.HeadErr = err }) }

// SetDeleteErr swaps the injected DeleteObject failure under the lock.
func (m *InMemory) SetDeleteErr(err error) { m.set(func() { m.DeleteErr = err }) }

// set runs fn holding the lock.
func (m *InMemory) set(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn()
}

// CopyCallCount returns how many times CopyObject was invoked, under the lock.
func (m *InMemory) CopyCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.CopyCalls
}

// LastDelete reports whether the most recent DeleteObject carried a deadline
// and what it was. Takes the lock, so it is safe to call while the system
// under test is still running.
func (m *InMemory) LastDelete() (hadDeadline bool, deadline time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.LastDeleteHadDeadline, m.LastDeleteDeadline
}

// PutBodyWasSeekable reports whether the most recent PutObject received a
// seekable body, under the lock.
func (m *InMemory) PutBodyWasSeekable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.LastPutBodySeekable
}

// Get returns the stored object and whether it was present, under the lock.
func (m *InMemory) Get(key string) (Object, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.Objects[key]
	return obj, ok
}

// Put stores obj directly, bypassing PutObject. Use it to seed state a test
// depends on without exercising the write path.
func (m *InMemory) Put(key string, obj *Object) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Objects[key] = *obj
}

// Len returns the number of stored objects.
func (m *InMemory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Objects)
}
