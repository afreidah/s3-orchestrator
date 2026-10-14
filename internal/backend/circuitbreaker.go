// -------------------------------------------------------------------------------
// CircuitBreakerBackend - Per-Backend and Per-Call Fault Isolation Wrapper
//
// Author: Alex Freidah
//
// Wraps an ObjectBackend in two layers of circuit breaker so a backend that is
// failing is excluded from routing, and a backend refusing only some calls
// keeps serving the rest. The backend breaker counts failures that say the
// whole backend is broken - no HTTP response at all, or a 401 - and while it
// is open no call reaches the backend. Each guarded call has its own breaker
// counting the backend's refusals of that call - a 403, a 429 or a 5xx - so a
// provider refusing downloads over its quota leaves uploads, deletes and
// listings working. A call is served while both its breaker and the backend
// breaker are closed, and the served calls are published as one
// core.BackendCalls.
//
// While open, every call is refused; each breaker recovers out of band through
// its own check (see RecoveryProber and Probes), never with a client request.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// probeKey is the one key the orchestrator writes to a backend for itself: the
// call probes list it, delete it and write it. No bucket name can start with
// a dot, so it falls under no bucket's prefix, and convertListPage drops it
// so nothing above this package ever lists it.
const probeKey = ".s3o-probe"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CircuitBreakerBackend wraps an ObjectBackend with circuit breaker
// protection. A call its breakers refuse returns ErrBackendUnavailable without
// touching the real backend.
type CircuitBreakerBackend struct {
	inner   CheckedBackend
	name    string
	backend *breaker.CircuitBreaker
	calls   map[core.BackendCalls]*callBreaker

	backendOpen atomic.Bool
	callsClosed atomic.Uint64

	mu            sync.Mutex
	openedAt      time.Time
	onStateChange func(breaker.StateChangeInfo)
}

// callBreaker is one guarded call's breaker and the key of the last request
// the backend refused, which the GET and HEAD probes retry.
type callBreaker struct {
	breaker *breaker.CircuitBreaker
	lastKey atomic.Pointer[string]
}

// Probe is one breaker's recovery check: the breaker it closes, the
// operation the check is admitted and charged as, and the check itself.
type Probe struct {
	Breaker *breaker.CircuitBreaker
	Op      s3op.Operation
	Check   func(context.Context) error
}

// Compile-time check.
var _ ObjectBackend = (*CircuitBreakerBackend)(nil)

// CircuitBreakerConfig configures the circuit breakers wrapping a backend.
type CircuitBreakerConfig struct {
	Name      string
	Threshold int
	Timeout   time.Duration
}

// failureScope is which breaker a failed call counts against.
type failureScope int

// The failure scopes.
const (
	scopeNone    failureScope = iota // caller-side or about one request; counts nowhere
	scopeBackend                     // the backend cannot be reached or rejects the credentials
	scopeCall                        // the backend answered and refused this call
)

// NewCircuitBreakerBackend wraps a backend with a backend breaker and one
// breaker per guarded call, each recovered only through Recover by the
// RecoveryProber running its Probe. Transitions of the served set surface on
// the standard CircuitBreaker* metrics and the BackendCircuit* events: the
// backend counts as open while any call is not served.
func NewCircuitBreakerBackend(inner CheckedBackend, cfg CircuitBreakerConfig) *CircuitBreakerBackend {
	cb := &CircuitBreakerBackend{
		inner:         inner,
		name:          cfg.Name,
		calls:         make(map[core.BackendCalls]*callBreaker),
		onStateChange: telemetry.NewCircuitBreakerHook(cfg.Name),
	}
	cb.callsClosed.Store(uint64(core.AllCalls))

	cb.backend = newBreaker(cfg, cfg.Name, scopeBackend)
	cb.backend.SetOnStateChange(func(info breaker.StateChangeInfo) {
		cb.apply(info, func() { cb.backendOpen.Store(info.To != breaker.StateClosed) })
	})

	// Each single bit of AllCalls, lowest first.
	for rest := uint64(core.AllCalls); rest != 0; rest &= rest - 1 {
		call := core.BackendCalls(rest & -rest)
		br := newBreaker(cfg, cfg.Name+"/"+call.String(), scopeCall)
		br.SetOnStateChange(func(info breaker.StateChangeInfo) {
			cb.apply(info, func() {
				if info.To == breaker.StateClosed {
					cb.callsClosed.Or(uint64(call))
				} else {
					cb.callsClosed.And(^uint64(call))
				}
			})
		})
		cb.calls[call] = &callBreaker{breaker: br}
	}
	return cb
}

// newBreaker builds one breaker of a backend, counting only failures of the
// given scope.
func newBreaker(cfg CircuitBreakerConfig, name string, scope failureScope) *breaker.CircuitBreaker {
	return breaker.NewCircuitBreaker(breaker.Config{
		Name:             name,
		Threshold:        cfg.Threshold,
		Timeout:          cfg.Timeout,
		IsError:          func(err error) bool { return classifyFailure(err) == scope },
		Sentinel:         breaker.ErrBackendUnavailable,
		ExternalRecovery: true,
	})
}

// classifyFailure says which breaker a failed call counts against. A failure
// with no HTTP status (connection refused, DNS, TLS, reset) or a 401 says the
// whole backend is broken. A 5xx, a 429 or a 403 says the backend refused this
// call, which a provider can do to one call alone, as a download quota does to
// GET. Any other status is about one request, and counting it would let a
// single bad object trip a healthy backend. Context cancellation and deadline
// are caller-side and never count, and neither does a provider declining batch
// delete, which is a missing capability rather than a fault, nor a refusal
// from one of this wrapper's own breakers.
func classifyFailure(err error) failureScope {
	if err == nil {
		return scopeNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrBatchDeleteNotSupported) || errors.Is(err, breaker.ErrBackendUnavailable) {
		return scopeNone
	}
	respErr, ok := errors.AsType[httpStatusError](err)
	if !ok {
		return scopeBackend
	}
	switch status := respErr.HTTPStatusCode(); {
	case status == http.StatusUnauthorized:
		return scopeBackend
	case status >= http.StatusInternalServerError, status == http.StatusTooManyRequests, status == http.StatusForbidden:
		return scopeCall
	default:
		return scopeNone
	}
}

// -------------------------------------------------------------------------
// SERVED CALLS
// -------------------------------------------------------------------------

// Name returns the backend's name.
func (cb *CircuitBreakerBackend) Name() string { return cb.name }

// Served returns the calls the backend is serving: every call whose breaker is
// closed, or none while the backend breaker is open.
func (cb *CircuitBreakerBackend) Served() core.BackendCalls {
	if cb.backendOpen.Load() {
		return 0
	}
	return core.BackendCalls(cb.callsClosed.Load())
}

// IsHealthy reports whether the backend serves every call.
func (cb *CircuitBreakerBackend) IsHealthy() bool {
	return cb.Served() == core.AllCalls
}

// State reports the backend as closed while it serves every call and open
// otherwise.
func (cb *CircuitBreakerBackend) State() breaker.State {
	if cb.IsHealthy() {
		return breaker.StateClosed
	}
	return breaker.StateOpen
}

// OpenDuration returns how long the backend has not served every call, or
// zero while it does.
func (cb *CircuitBreakerBackend) OpenDuration() time.Duration {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.IsHealthy() {
		return 0
	}
	return time.Since(cb.openedAt)
}

// apply runs one breaker's transition against the served set and, when that
// changes whether the backend serves every call, reports a backend-level
// transition carrying the failure counts of the breaker that caused it. Runs
// inside the transitioning breaker's lock, so it reads no breaker.
func (cb *CircuitBreakerBackend) apply(info breaker.StateChangeInfo, update func()) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	wasHealthy := cb.IsHealthy()
	update()
	isHealthy := cb.IsHealthy()
	if wasHealthy == isHealthy {
		return
	}

	info.Name = cb.name
	if isHealthy {
		info.From, info.To = breaker.StateOpen, breaker.StateClosed
		info.OpenDuration = time.Since(cb.openedAt)
	} else {
		info.From, info.To = breaker.StateClosed, breaker.StateOpen
		info.OpenDuration = 0
		cb.openedAt = time.Now()
	}
	if cb.onStateChange != nil {
		cb.onStateChange(info)
	}
}

// -------------------------------------------------------------------------
// GUARDED CALLS
// -------------------------------------------------------------------------

// guard runs fn through the backend breaker and then call's own breaker.
func guard[T any](cb *CircuitBreakerBackend, call core.BackendCalls, fn func() (T, error)) (T, error) {
	return cb.backend.Call(func() (T, error) {
		return cb.calls[call].breaker.Call(fn)
	})
}

// guardErr is guard for a call that returns only an error.
func guardErr(cb *CircuitBreakerBackend, call core.BackendCalls, fn func() error) error {
	_, err := guard(cb, call, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// noteRefusal records key as the last one the backend refused call for, so
// the call's probe retries a key that exists on the backend.
func (c *callBreaker) noteRefusal(key string, err error) {
	if classifyFailure(err) == scopeCall {
		c.lastKey.Store(&key)
	}
}

// PutObject uploads an object to the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	return guard(cb, core.CallPutObject, func() (string, error) {
		return cb.inner.PutObject(ctx, key, body, size, contentType, metadata)
	})
}

// GetObject retrieves an object from the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	return guard(cb, core.CallGetObject, func() (*GetObjectResult, error) {
		res, err := cb.inner.GetObject(ctx, key, rangeHeader)
		cb.calls[core.CallGetObject].noteRefusal(key, err)
		return res, err
	})
}

// HeadObject retrieves object metadata with circuit breaker protection.
func (cb *CircuitBreakerBackend) HeadObject(ctx context.Context, key string) (*HeadObjectResult, error) {
	return guard(cb, core.CallHeadObject, func() (*HeadObjectResult, error) {
		res, err := cb.inner.HeadObject(ctx, key)
		cb.calls[core.CallHeadObject].noteRefusal(key, err)
		return res, err
	})
}

// DeleteObject removes an object from the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) DeleteObject(ctx context.Context, key string) error {
	return guardErr(cb, core.CallDeleteObject, func() error {
		return cb.inner.DeleteObject(ctx, key)
	})
}

// CopyObject forwards a server-side copy through the circuit breakers, or
// returns ErrCopyNotSupported when the wrapped backend does not implement
// Copier.
func (cb *CircuitBreakerBackend) CopyObject(ctx context.Context, srcKey, dstKey, contentType string, metadata map[string]string) (string, error) {
	copier, ok := cb.inner.(Copier)
	if !ok {
		return "", ErrCopyNotSupported
	}
	return guard(cb, core.CallCopyObject, func() (string, error) {
		return copier.CopyObject(ctx, srcKey, dstKey, contentType, metadata)
	})
}

// DeleteObjects forwards a batch delete through the circuit breakers, or
// returns ErrBatchDeleteNotSupported when the wrapped backend does not
// implement BatchDeleter. Only a failed request counts toward a breaker; keys
// the backend declined individually do not.
func (cb *CircuitBreakerBackend) DeleteObjects(ctx context.Context, keys []string) (map[string]error, error) {
	deleter, ok := cb.inner.(BatchDeleter)
	if !ok {
		return nil, ErrBatchDeleteNotSupported
	}
	return guard(cb, core.CallDeleteObjects, func() (map[string]error, error) {
		return deleter.DeleteObjects(ctx, keys)
	})
}

// ListObjects lists objects by prefix with circuit breaker protection.
func (cb *CircuitBreakerBackend) ListObjects(ctx context.Context, prefix string, fn func([]ListedObject) error) error {
	return guardErr(cb, core.CallListObjects, func() error {
		return cb.inner.ListObjects(ctx, prefix, fn)
	})
}

// -------------------------------------------------------------------------
// RECOVERY PROBES
// -------------------------------------------------------------------------

// Probes returns the recovery check of every breaker the backend has. The
// backend breaker checks the bucket, so every call comes back once the backend
// answers. Each call breaker repeats its own call against the backend
// directly, bypassing the breakers, and passes unless refused again.
func (cb *CircuitBreakerBackend) Probes() []Probe {
	probes := []Probe{{Breaker: cb.backend, Op: s3op.HeadBucket, Check: cb.inner.HeadBucket}}
	for call, c := range cb.calls {
		probes = append(probes, Probe{
			Breaker: c.breaker,
			Op:      s3op.Operation(call.String()),
			Check:   cb.callCheck(call),
		})
	}
	return probes
}

// callCheck returns the probe for one call. GET and HEAD retry the last key
// the backend refused; the others use probeKey, which PUT and copy write.
func (cb *CircuitBreakerBackend) callCheck(call core.BackendCalls) func(context.Context) error {
	switch call {
	case core.CallGetObject:
		return func(ctx context.Context) error {
			res, err := cb.inner.GetObject(ctx, cb.calls[call].probeTarget(), "bytes=0-0")
			if err == nil {
				_ = res.Body.Close()
			}
			return probeResult(err)
		}
	case core.CallHeadObject:
		return func(ctx context.Context) error {
			_, err := cb.inner.HeadObject(ctx, cb.calls[call].probeTarget())
			return probeResult(err)
		}
	case core.CallListObjects:
		return func(ctx context.Context) error {
			return probeResult(cb.inner.ListObjects(ctx, probeKey, func([]ListedObject) error { return ErrStopListing }))
		}
	case core.CallDeleteObject:
		return func(ctx context.Context) error {
			return probeResult(cb.inner.DeleteObject(ctx, probeKey))
		}
	case core.CallDeleteObjects:
		return func(ctx context.Context) error {
			deleter, ok := cb.inner.(BatchDeleter)
			if !ok {
				return ErrBatchDeleteNotSupported
			}
			_, err := deleter.DeleteObjects(ctx, []string{probeKey})
			return probeResult(err)
		}
	case core.CallPutObject, core.CallCopyObject:
		return func(ctx context.Context) error {
			_, err := cb.inner.PutObject(ctx, probeKey, strings.NewReader(""), 0, "application/octet-stream", nil)
			return probeResult(err)
		}
	}
	return nil
}

// probeTarget is the key a GET or HEAD probe asks for: the last one the
// backend refused, or probeKey before any was.
func (c *callBreaker) probeTarget() string {
	if key := c.lastKey.Load(); key != nil {
		return *key
	}
	return probeKey
}

// probeResult reduces a call probe's error to whether the call is still
// refused. The probe passes on any answer its breaker would not count, such as
// a 404 for a key deleted since it was refused, and fails on a refusal, on a
// failure to reach the backend, and on running out of time.
func probeResult(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if classifyFailure(err) == scopeNone {
		return nil
	}
	return err
}
