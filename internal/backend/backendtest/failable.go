// -------------------------------------------------------------------------------
// FailableBackend - Injectable Failure Wrapper for ObjectBackend
//
// Author: Alex Freidah
//
// Wraps an ObjectBackend (typically a real or mock S3Backend) and can be
// toggled to return injected errors per-method. Mirrors the FailableStore
// pattern used for integration tests that exercise database-circuit-breaker
// paths — FailableBackend unlocks equivalent scenarios on the object-store
// side (backend 500s, timeouts, per-operation errors, mid-stream read
// failures, etc.).
//
// Thread-safe: all toggles and error injections are guarded by an internal
// RWMutex, so tests can flip failure modes from any goroutine.
// -------------------------------------------------------------------------------

package backendtest

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/backend"
)

// ErrSimulatedBackendFailure is the default error returned when no custom
// error is configured for a method. Tests can call SetErr with a more
// specific sentinel (e.g. a specific S3Error) when the test needs to
// distinguish failure modes.
var ErrSimulatedBackendFailure = errors.New("simulated backend failure")

// Method identifies which ObjectBackend operation a failure toggle applies
// to. Tests reference the method constants directly rather than using string
// keys so typos surface as compile errors.
type Method int

const (
	MethodPut Method = iota
	MethodGet
	MethodHead
	MethodDelete
)

// FailableBackend wraps an ObjectBackend and injects configurable errors.
// By default all methods pass through to the wrapped backend. Call SetFailing
// to fail all methods with a generic error, or SetErr for per-method control.
type FailableBackend struct {
	backend.ObjectBackend

	mu       sync.RWMutex
	failAll  bool
	failAllErr error
	perMethod map[Method]error
}

// New wraps the given backend. The returned FailableBackend passes through
// every call until a failure is configured.
func New(inner backend.ObjectBackend) *FailableBackend {
	return &FailableBackend{
		ObjectBackend: inner,
		perMethod:     make(map[Method]error),
	}
}

// SetFailing toggles "fail every method" mode. The err argument is the error
// returned from every method; pass nil to use ErrSimulatedBackendFailure.
// Passing failing=false clears all failure modes, both global and per-method.
func (f *FailableBackend) SetFailing(failing bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAll = failing
	if err == nil {
		f.failAllErr = ErrSimulatedBackendFailure
	} else {
		f.failAllErr = err
	}
	if !failing {
		f.failAllErr = nil
		f.perMethod = make(map[Method]error)
	}
}

// SetErr installs a per-method error override. A method with a configured
// error fails with that error on every call until cleared (pass nil err to
// clear just this method). Global SetFailing takes precedence.
func (f *FailableBackend) SetErr(m Method, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.perMethod, m)
		return
	}
	f.perMethod[m] = err
}

// errFor returns the error that the given method should inject, or nil to
// pass through. Applies global failure first, then per-method override.
func (f *FailableBackend) errFor(m Method) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.failAll {
		return f.failAllErr
	}
	if err, ok := f.perMethod[m]; ok {
		return err
	}
	return nil
}

// -------------------------------------------------------------------------
// ObjectBackend implementation with failure injection
// -------------------------------------------------------------------------

func (f *FailableBackend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	if err := f.errFor(MethodPut); err != nil {
		return "", err
	}
	return f.ObjectBackend.PutObject(ctx, key, body, size, contentType, metadata)
}

func (f *FailableBackend) GetObject(ctx context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	if err := f.errFor(MethodGet); err != nil {
		return nil, err
	}
	return f.ObjectBackend.GetObject(ctx, key, rangeHeader)
}

func (f *FailableBackend) HeadObject(ctx context.Context, key string) (*backend.HeadObjectResult, error) {
	if err := f.errFor(MethodHead); err != nil {
		return nil, err
	}
	return f.ObjectBackend.HeadObject(ctx, key)
}

func (f *FailableBackend) DeleteObject(ctx context.Context, key string) error {
	if err := f.errFor(MethodDelete); err != nil {
		return err
	}
	return f.ObjectBackend.DeleteObject(ctx, key)
}
