// -------------------------------------------------------------------------------
// BackendRuntime Composition Tests
//
// Author: Alex Freidah
//
// Pins the forwarding contract of *BackendRuntime: every public method routes to
// the right capability service and returns what the underlying service
// returns. Complements capabilities_test.go (which tests each capability
// in isolation) by exercising the composition layer end-to-end so the
// thin-forward layer cannot silently regress (e.g., a forward to the
// wrong service or a missing forward).
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fakeBackend is a no-op backend used by BackendRuntime forwarder tests. None of
// its methods are exercised here; the type only needs to satisfy the
// interface so the registry holds something.
type fakeBackend struct{}

func (fakeBackend) PutObject(context.Context, string, io.Reader, int64, string, map[string]string) (string, error) {
	return "", nil
}
func (fakeBackend) GetObject(context.Context, string, string) (*backend.GetObjectResult, error) {
	return nil, errors.New("not implemented")
}
func (fakeBackend) HeadObject(context.Context, string) (*backend.HeadObjectResult, error) {
	return nil, errors.New("not implemented")
}
func (fakeBackend) DeleteObject(context.Context, string) error { return nil }

// newTestCore constructs a *BackendRuntime with sensible defaults so the
// forwarder tests focus on behavior, not wiring boilerplate.
func newTestCore(t *testing.T) *BackendRuntime {
	t.Helper()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	return New(&Config{
		Backends:        map[string]backend.ObjectBackend{"b1": fakeBackend{}, "b2": fakeBackend{}},
		Order:           []string{"b1", "b2"},
		BackendTimeout:  100 * time.Millisecond,
		Usage:           tracker,
		RoutingStrategy: config.RoutingPack,
		MaxObjectSizes:  map[string]int64{"b2": 1024},
		AdmissionSem:    make(chan struct{}, 2),
		Log:             slog.Default(),
	})
}

// -------------------------------------------------------------------------
// CONSTRUCTION + ACCESSORS
// -------------------------------------------------------------------------

func TestCore_New_ExposesAcctAndRoutingStrategy(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	if c.Acct() == nil {
		t.Error("Acct() returned nil after New")
	}
	if c.RoutingStrategy() != config.RoutingPack {
		t.Errorf("RoutingStrategy = %q, want %q", c.RoutingStrategy(), config.RoutingPack)
	}
	if c.Log() == nil {
		t.Error("Log() returned nil")
	}
}

// -------------------------------------------------------------------------
// BACKEND REGISTRY FORWARDERS
// -------------------------------------------------------------------------

func TestCore_BackendForwarders(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)

	if _, err := c.GetBackend("b1"); err != nil {
		t.Errorf("GetBackend(b1) returned err: %v", err)
	}
	if len(c.Backends()) != 2 {
		t.Errorf("Backends len = %d, want 2", len(c.Backends()))
	}
	if order := c.BackendOrder(); len(order) != 2 || order[0] != "b1" {
		t.Errorf("BackendOrder = %v, want [b1 b2]", order)
	}
}

func TestCore_DrainForwarders_NoCheckerThenWired(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	if c.IsDraining("b1") {
		t.Error("IsDraining(b1) = true before any checker wired")
	}
	c.SetDrainChecker(staticDrainChecker{"b1": true})
	if !c.IsDraining("b1") {
		t.Error("IsDraining(b1) = false after wiring drain checker")
	}
	if got := c.ExcludeDraining([]string{"b1", "b2"}); len(got) != 1 || got[0] != "b2" {
		t.Errorf("ExcludeDraining = %v, want [b2]", got)
	}
}

// -------------------------------------------------------------------------
// CIRCUIT BREAKER (registry.ExcludeUnhealthy)
// -------------------------------------------------------------------------

func TestCore_ExcludeUnhealthy_DropsOpenBreakers(t *testing.T) {
	t.Parallel()
	// Build a CB-wrapped backend whose underlying call fails repeatedly
	// so the breaker trips.
	cb := backend.NewCircuitBreakerBackend(fakeBackend{}, backend.CircuitBreakerConfig{Name: "flaky", Threshold: 1, Timeout: time.Hour})
	c := New(&Config{
		Backends: map[string]backend.ObjectBackend{"healthy": fakeBackend{}, "flaky": cb},
		Order:    []string{"healthy", "flaky"},
		Usage:    counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"healthy", "flaky"}), nil),
	})
	// Trip the breaker: call GetObject so it observes a backend error.
	for range 3 {
		_, _ = cb.GetObject(context.Background(), "k", "")
	}
	if cb.State() != breaker.StateOpen {
		t.Fatalf("breaker state = %v, want Open", cb.State())
	}
	got := c.ExcludeUnhealthy([]string{"healthy", "flaky"})
	if len(got) != 1 || got[0] != "healthy" {
		t.Errorf("ExcludeUnhealthy = %v, want [healthy]", got)
	}
}

// -------------------------------------------------------------------------
// USAGE FORWARDERS
// -------------------------------------------------------------------------

func TestCore_UsageForwarders(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	if c.Usage() == nil {
		t.Error("Usage() returned nil")
	}
	if got := c.MaxObjectSize("b2"); got != 1024 {
		t.Errorf("MaxObjectSize(b2) = %d, want 1024", got)
	}
	if got := c.MaxObjectSize("b1"); got != 0 {
		t.Errorf("MaxObjectSize(b1) = %d, want 0 (unset)", got)
	}
}

func TestCore_EligibleForWrite_AppliesAllFilters(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	c.SetDrainChecker(staticDrainChecker{"b1": true})

	// b1 is draining -> excluded. b2 has max 1024; ingress 2048 -> excluded.
	if got := c.EligibleForWrite(1, 0, 2048); len(got) != 0 {
		t.Errorf("EligibleForWrite(2048) = %v, want []", got)
	}
	// b1 still draining; b2 within limit (ingress 512 < 1024) -> [b2].
	if got := c.EligibleForWrite(1, 0, 512); len(got) != 1 || got[0] != "b2" {
		t.Errorf("EligibleForWrite(512) = %v, want [b2]", got)
	}
}

// -------------------------------------------------------------------------
// TIMEOUT FORWARDERS
// -------------------------------------------------------------------------

// recordingBackend captures DeleteObject + PutObject calls so the
// timeout forwarders can be exercised without spinning up real I/O.
type recordingBackend struct {
	fakeBackend
	delErr     error
	deleted    []string
	putBodyLen int64
}

func (r *recordingBackend) DeleteObject(_ context.Context, key string) error {
	r.deleted = append(r.deleted, key)
	return r.delErr
}
func (r *recordingBackend) PutObject(_ context.Context, _ string, body io.Reader, size int64, _ string, _ map[string]string) (string, error) {
	r.putBodyLen = size
	_, _ = io.Copy(io.Discard, body)
	return "etag", nil
}

// readableBackend has a GetObject that returns a non-empty body so
// StreamCopy can drive the read leg without external setup.
type readableBackend struct {
	fakeBackend
	payload []byte
	getErr  error
}

func (r *readableBackend) GetObject(_ context.Context, _ string, _ string) (*backend.GetObjectResult, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return &backend.GetObjectResult{
		Body: io.NopCloser(bytesReader(r.payload)),
		Size: int64(len(r.payload)),
	}, nil
}

// bytesReader wraps a byte slice in an io.Reader without pulling in
// bytes.NewReader to keep the import list minimal.
func bytesReader(b []byte) io.Reader {
	return &byteSliceReader{data: b}
}

type byteSliceReader struct {
	data []byte
	pos  int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestCore_StreamCopy_ReadSuccessThenWriteSuccess(t *testing.T) {
	t.Parallel()
	src := &readableBackend{payload: []byte("hello")}
	dst := &recordingBackend{}
	c := newTestCore(t)
	if err := c.StreamCopy(context.Background(), src, dst, "k"); err != nil {
		t.Fatalf("StreamCopy: %v", err)
	}
	if dst.putBodyLen != 5 {
		t.Errorf("dst received %d bytes, want 5", dst.putBodyLen)
	}
}

func TestCore_StreamCopy_TagsReadPhaseOnGetError(t *testing.T) {
	t.Parallel()
	src := &readableBackend{getErr: errors.New("upstream gone")}
	dst := &recordingBackend{}
	c := newTestCore(t)
	err := c.StreamCopy(context.Background(), src, dst, "k")
	ce, ok := errors.AsType[*backend.CopyError](err)
	if !ok {
		t.Fatalf("err = %v, want *backend.CopyError", err)
	}
	if ce.Phase != backend.CopyPhaseRead {
		t.Errorf("phase = %v, want CopyPhaseRead", ce.Phase)
	}
}

// errReader delivers a prefix then fails with err, simulating a source body
// that stalls or breaks mid-stream (a degraded source returning 200 then
// dying). The destination's PutObject reads through it and observes err.
type errReader struct {
	prefix []byte
	pos    int
	err    error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.pos < len(r.prefix) {
		n := copy(p, r.prefix[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

// stallingSourceBackend's GetObject succeeds but hands back a body that errors
// mid-read, so the failure only surfaces while the destination is uploading.
type stallingSourceBackend struct {
	fakeBackend
	err error
}

func (s *stallingSourceBackend) GetObject(_ context.Context, _ string, _ string) (*backend.GetObjectResult, error) {
	return &backend.GetObjectResult{
		Body: io.NopCloser(&errReader{prefix: []byte("partial"), err: s.err}),
		Size: 100,
	}, nil
}

// propagatingDstBackend uploads by draining the body and surfaces any read
// error, like a real backend whose PUT fails when the source stream breaks.
type propagatingDstBackend struct{ fakeBackend }

func (propagatingDstBackend) PutObject(_ context.Context, _ string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return "", err
	}
	return "etag", nil
}

// writeRejectingBackend drains the body cleanly (the source is fine) then
// rejects the write with its own error, e.g. AccessDenied on the target.
type writeRejectingBackend struct{ fakeBackend }

func (writeRejectingBackend) PutObject(_ context.Context, _ string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	_, _ = io.Copy(io.Discard, body)
	return "", errors.New("access denied")
}

// TestCore_StreamCopy_SourceStallTaggedReadPhase verifies a source body that
// fails mid-read is attributed to the read phase even though the error
// surfaces from the destination's PutObject - so a degraded source makes the
// copier retry another source instead of tripping the healthy target's breaker.
func TestCore_StreamCopy_SourceStallTaggedReadPhase(t *testing.T) {
	t.Parallel()
	src := &stallingSourceBackend{err: context.DeadlineExceeded}
	dst := propagatingDstBackend{}
	c := newTestCore(t)
	err := c.StreamCopy(context.Background(), src, dst, "k")
	ce, ok := errors.AsType[*backend.CopyError](err)
	if !ok {
		t.Fatalf("err = %v, want *backend.CopyError", err)
	}
	if ce.Phase != backend.CopyPhaseRead {
		t.Errorf("phase = %v, want CopyPhaseRead (a source stall must not blame the target)", ce.Phase)
	}
}

// TestCore_StreamCopy_GenuineWriteErrorTaggedWritePhase verifies the
// read-tracker does not over-correct: when the body reads cleanly and the
// target itself rejects the write, the failure is still tagged write phase
// (terminal) so the copier does not pointlessly retry other sources.
func TestCore_StreamCopy_GenuineWriteErrorTaggedWritePhase(t *testing.T) {
	t.Parallel()
	src := &readableBackend{payload: []byte("hello")}
	dst := writeRejectingBackend{}
	c := newTestCore(t)
	err := c.StreamCopy(context.Background(), src, dst, "k")
	ce, ok := errors.AsType[*backend.CopyError](err)
	if !ok {
		t.Fatalf("err = %v, want *backend.CopyError", err)
	}
	if ce.Phase != backend.CopyPhaseWrite {
		t.Errorf("phase = %v, want CopyPhaseWrite (a real target rejection stays terminal)", ce.Phase)
	}
}

func TestCore_DeleteWithTimeout_ForwardsToBackend(t *testing.T) {
	t.Parallel()
	rb := &recordingBackend{}
	c := newTestCore(t)
	if err := c.DeleteWithTimeout(context.Background(), rb, "k"); err != nil {
		t.Fatalf("DeleteWithTimeout: %v", err)
	}
	if len(rb.deleted) != 1 || rb.deleted[0] != "k" {
		t.Errorf("recorded deletes = %v, want [k]", rb.deleted)
	}
}

func TestCore_WithTimeout_ProducesUsableCancel(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	ctx, cancel := c.WithTimeout(context.Background())
	if ctx == nil || cancel == nil {
		t.Fatal("WithTimeout returned nil ctx or nil cancel")
	}
	cancel()
	if ctx.Err() == nil {
		t.Error("cancel() did not cancel returned ctx")
	}
}

// -------------------------------------------------------------------------
// ERROR CLASSIFIER FORWARDER
// -------------------------------------------------------------------------

func TestCore_ClassifyWriteError_Forwards(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	if err := c.ClassifyWriteError(span, "PutObject", core.ErrDBUnavailable); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Errorf("classify(DBUnavailable) = %v, want ErrServiceUnavailable", err)
	}
}

// -------------------------------------------------------------------------
// ADMISSION FORWARDERS
// -------------------------------------------------------------------------

func TestCore_AdmissionForwarders(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	if c.AdmissionSem() == nil {
		t.Error("AdmissionSem() returned nil despite sem wired in config")
	}
	if !c.AcquireAdmission(context.Background()) {
		t.Fatal("first AcquireAdmission failed")
	}
	if !c.AcquireAdmission(context.Background()) {
		t.Fatal("second AcquireAdmission (sem cap 2) failed")
	}
	// Third should fail under a cancelled ctx.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.AcquireAdmission(cctx) {
		t.Error("third AcquireAdmission succeeded against full sem with cancelled ctx")
	}
	c.ReleaseAdmission()
	c.ReleaseAdmission()
}
