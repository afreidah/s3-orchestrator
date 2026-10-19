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
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
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
func (fakeBackend) HeadBucket(context.Context) error           { return nil }
func (fakeBackend) ListObjects(context.Context, string, func([]backend.ListedObject) error) error {
	return nil
}

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

func TestCore_StateForwarders_NoCheckerThenWired(t *testing.T) {
	t.Parallel()
	c := newTestCore(t)
	if c.BackendFlags("b1") != 0 {
		t.Error("b1 has flags before any checker wired")
	}
	c.SetStateChecker(staticFlags{"b1": core.FlagDraining})
	if c.BackendFlags("b1") != core.FlagDraining {
		t.Errorf("BackendFlags(b1) = %s, want draining", c.BackendFlags("b1"))
	}
	if got := c.Accepted([]string{"b1", "b2"}, core.AcceptForWrite); len(got) != 1 || got[0] != "b2" {
		t.Errorf("Accepted = %v, want [b2]", got)
	}
}

// -------------------------------------------------------------------------
// CIRCUIT BREAKER (registry.Serving)
// -------------------------------------------------------------------------

// TestCore_Serving_DropsRefusingBackends verifies a backend whose breaker is
// open is dropped and reports how long it has refused, while an unwrapped
// backend serves every call and an unknown one serves none.
func TestCore_Serving_DropsRefusingBackends(t *testing.T) {
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
	got := c.Serving([]string{"healthy", "flaky"}, core.RequiredForWrite)
	if len(got) != 1 || got[0] != "healthy" {
		t.Errorf("Serving = %v, want [healthy]", got)
	}
	if served := c.ServedCalls("healthy"); served != core.AllCalls {
		t.Errorf("ServedCalls(healthy) = %s, want every call", served)
	}
	if served := c.ServedCalls("missing"); served != 0 {
		t.Errorf("ServedCalls(missing) = %s, want none", served)
	}
	if c.RefusedFor("flaky", core.RequiredForRead) == 0 {
		t.Error("RefusedFor(flaky) = 0 with its breaker open")
	}
	if d := c.RefusedFor("healthy", core.RequiredForRead); d != 0 {
		t.Errorf("RefusedFor(healthy) = %v, want 0", d)
	}
}

// TestCore_WriteTargets verifies a backend takes new bytes only while its flags
// accept writes and it serves both uploads and downloads: one refusing reads
// is left out, as is a cordoned one, and the rest keep their configured order.
func TestCore_WriteTargets(t *testing.T) {
	t.Parallel()
	names := []string{"cordoned", "healthy", "no-reads", "also-healthy"}
	c := New(&Config{
		Backends: map[string]backend.ObjectBackend{
			"cordoned":     fakeBackend{},
			"healthy":      fakeBackend{},
			"no-reads":     backendtest.RefusingReads("no-reads"),
			"also-healthy": fakeBackend{},
		},
		Order: names,
		Usage: counter.NewUsageTracker(counter.NewLocalCounterBackend(names), nil),
	})
	c.SetStateChecker(staticFlags{"cordoned": core.FlagCordoned})

	got := c.WriteTargets()
	if want := []string{"healthy", "also-healthy"}; !slices.Equal(got, want) {
		t.Errorf("WriteTargets = %v, want %v", got, want)
	}
	if eligible := c.EligibleForWrite([]s3op.Operation{s3op.PutObject}, 0, 1); !slices.Equal(eligible, got) {
		t.Errorf("EligibleForWrite = %v, want the write targets %v", eligible, got)
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
	c.SetStateChecker(staticFlags{"b1": core.FlagDraining})

	// b1 is draining -> excluded. b2 has max 1024; ingress 2048 -> excluded.
	if got := c.EligibleForWrite([]s3op.Operation{s3op.PutObject}, 0, 2048); len(got) != 0 {
		t.Errorf("EligibleForWrite(2048) = %v, want []", got)
	}
	// b1 still draining; b2 within limit (ingress 512 < 1024) -> [b2].
	if got := c.EligibleForWrite([]s3op.Operation{s3op.PutObject}, 0, 512); len(got) != 1 || got[0] != "b2" {
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

// ep names a backend for a stream copy. These tests exercise the copy legs
// rather than the accounting, so the names only have to be distinct and carry
// no configured limits, which is what leaves every transfer admitted.
func ep(name string, be backend.ObjectBackend) backend.CopyEndpoint {
	return backend.CopyEndpoint{Name: name, Backend: be}
}

// copyReq is a verbatim copy of k on src to k!dst on dst, admitted against
// sizeEstimate.
func copyReq(src, dst backend.CopyEndpoint, sizeEstimate int64) *backend.CopyRequest {
	return &backend.CopyRequest{Src: src, Dst: dst, SrcKey: "k", DstKey: "k!dst", SizeEstimate: sizeEstimate}
}

func TestCore_StreamCopy_ReadSuccessThenWriteSuccess(t *testing.T) {
	t.Parallel()
	src := &readableBackend{payload: []byte("hello")}
	dst := &recordingBackend{}
	c := newTestCore(t)
	if _, err := c.StreamCopy(context.Background(), copyReq(ep("src", src), ep("dst", dst), 0)); err != nil {
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
	_, err := c.StreamCopy(context.Background(), copyReq(ep("src", src), ep("dst", dst), 0))
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
	_, err := c.StreamCopy(context.Background(), copyReq(ep("src", src), ep("dst", dst), 0))
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
	_, err := c.StreamCopy(context.Background(), copyReq(ep("src", src), ep("dst", dst), 0))
	ce, ok := errors.AsType[*backend.CopyError](err)
	if !ok {
		t.Fatalf("err = %v, want *backend.CopyError", err)
	}
	if ce.Phase != backend.CopyPhaseWrite {
		t.Errorf("phase = %v, want CopyPhaseWrite (a real target rejection stays terminal)", ce.Phase)
	}
}

// limitedCore builds a runtime where b1 and b2 carry the given usage limits,
// so a copy between them can be pushed past one side's budget.
func limitedCore(t *testing.T, limits map[string]core.UsageLimits, spent map[string]core.UsageStat) *BackendRuntime {
	t.Helper()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	tracker.UpdateLimits(limits)
	// Byte budgets only here, so there is no pool baseline to seed.
	baselines := make(map[string]core.Usage, len(spent))
	for name, stat := range spent {
		baselines[name] = core.Usage{UsageStat: stat}
	}
	tracker.SetBaselines(baselines)
	return New(&Config{
		Backends:        map[string]backend.ObjectBackend{"b1": fakeBackend{}, "b2": fakeBackend{}},
		Order:           []string{"b1", "b2"},
		BackendTimeout:  100 * time.Millisecond,
		Usage:           tracker,
		RoutingStrategy: config.RoutingPack,
		AdmissionSem:    make(chan struct{}, 2),
		Log:             slog.Default(),
	})
}

// TestCore_StreamCopy_RefusesSourceOutOfEgress asserts a copy is refused when
// the source has no egress headroom, so a repair cannot spend a source's budget
// that client reads need. The refusal is tagged read-phase because another
// source may still have egress left.
func TestCore_StreamCopy_RefusesSourceOutOfEgress(t *testing.T) {
	t.Parallel()
	src := &readableBackend{payload: []byte("hello")}
	dst := &recordingBackend{}
	c := limitedCore(t,
		map[string]core.UsageLimits{"b1": {EgressByteLimit: 100}},
		map[string]core.UsageStat{"b1": {EgressBytes: 99}})

	_, err := c.StreamCopy(context.Background(), copyReq(ep("b1", src), ep("b2", dst), 50))
	if !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("err = %v, want core.ErrUsageLimitExceeded", err)
	}
	ce, ok := errors.AsType[*backend.CopyError](err)
	if !ok {
		t.Fatalf("err = %v, want *backend.CopyError", err)
	}
	if ce.Phase != backend.CopyPhaseRead {
		t.Errorf("phase = %v, want CopyPhaseRead so another source is still tried", ce.Phase)
	}
	if dst.putBodyLen != 0 {
		t.Errorf("destination received %d bytes; a refused copy must not touch either backend", dst.putBodyLen)
	}
}

// TestCore_StreamCopy_RefusesDestinationOutOfIngress covers the other leg. A
// full destination is terminal: no other source improves it, so the refusal is
// tagged write-phase and the copier stops rather than working through every
// remaining source.
func TestCore_StreamCopy_RefusesDestinationOutOfIngress(t *testing.T) {
	t.Parallel()
	src := &readableBackend{payload: []byte("hello")}
	dst := &recordingBackend{}
	c := limitedCore(t,
		map[string]core.UsageLimits{"b2": {IngressByteLimit: 100}},
		map[string]core.UsageStat{"b2": {IngressBytes: 99}})

	_, err := c.StreamCopy(context.Background(), copyReq(ep("b1", src), ep("b2", dst), 50))
	if !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("err = %v, want core.ErrUsageLimitExceeded", err)
	}
	ce, ok := errors.AsType[*backend.CopyError](err)
	if !ok {
		t.Fatalf("err = %v, want *backend.CopyError", err)
	}
	if ce.Phase != backend.CopyPhaseWrite {
		t.Errorf("phase = %v, want CopyPhaseWrite so the attempt ends", ce.Phase)
	}
}

// TestCore_StreamCopy_AdmitsWithinLimits pins that admission does not refuse a
// copy that fits, which is the case every healthy fleet is in.
func TestCore_StreamCopy_AdmitsWithinLimits(t *testing.T) {
	t.Parallel()
	src := &readableBackend{payload: []byte("hello")}
	dst := &recordingBackend{}
	c := limitedCore(t,
		map[string]core.UsageLimits{
			"b1": {EgressByteLimit: 1 << 20},
			"b2": {IngressByteLimit: 1 << 20},
		}, nil)

	moved, err := c.StreamCopy(context.Background(), copyReq(ep("b1", src), ep("b2", dst), 5))
	if err != nil {
		t.Fatalf("StreamCopy: %v", err)
	}
	if moved != 5 || dst.putBodyLen != 5 {
		t.Errorf("moved = %d, destination got %d bytes, want 5 and 5", moved, dst.putBodyLen)
	}
}

// newCopyRuntime builds a runtime over b1 and b2 with no usage limits, and
// returns the counters its copies are charged to.
func newCopyRuntime(t *testing.T) (*BackendRuntime, *counter.LocalCounterBackend) {
	t.Helper()
	counters := counter.NewLocalCounterBackend([]string{"b1", "b2"})
	return New(&Config{
		Backends:        map[string]backend.ObjectBackend{"b1": fakeBackend{}, "b2": fakeBackend{}},
		Order:           []string{"b1", "b2"},
		BackendTimeout:  100 * time.Millisecond,
		Usage:           counter.NewUsageTracker(counters, nil),
		RoutingStrategy: config.RoutingPack,
		Log:             slog.Default(),
	}), counters
}

// usageOf is one backend's charged calls, egress and ingress.
type usageOf struct {
	calls, egress, ingress int64
}

// loadUsage reads backend's charges from counters.
func loadUsage(counters *counter.LocalCounterBackend, backend string) usageOf {
	var u core.Usage
	counters.Load(backend, &u)
	return usageOf{calls: u.APIRequests, egress: u.EgressBytes, ingress: u.IngressBytes}
}

// TestCore_StreamCopy_ChargesBothLegs verifies a successful copy charges the
// source one call and the bytes read, and the destination one call and the
// bytes written.
func TestCore_StreamCopy_ChargesBothLegs(t *testing.T) {
	t.Parallel()
	c, counters := newCopyRuntime(t)
	src := &readableBackend{payload: []byte("hello")}

	if _, err := c.StreamCopy(context.Background(), copyReq(ep("b1", src), ep("b2", &recordingBackend{}), 5)); err != nil {
		t.Fatalf("StreamCopy: %v", err)
	}
	if got := loadUsage(counters, "b1"); got != (usageOf{calls: 1, egress: 5}) {
		t.Errorf("source charged %+v, want one call and 5 bytes out", got)
	}
	if got := loadUsage(counters, "b2"); got != (usageOf{calls: 1, ingress: 5}) {
		t.Errorf("destination charged %+v, want one call and 5 bytes in", got)
	}
}

// TestCore_StreamCopy_ChargesFailedLegs verifies a failed read charges the
// source a call and leaves the destination alone, and a failed write charges
// the source its read and the destination a call with no bytes.
func TestCore_StreamCopy_ChargesFailedLegs(t *testing.T) {
	t.Parallel()
	t.Run("read fails", func(t *testing.T) {
		t.Parallel()
		c, counters := newCopyRuntime(t)
		src := &readableBackend{getErr: errors.New("upstream gone")}
		if _, err := c.StreamCopy(context.Background(), copyReq(ep("b1", src), ep("b2", &recordingBackend{}), 5)); err == nil {
			t.Fatal("StreamCopy succeeded, want the read failure")
		}
		if got := loadUsage(counters, "b1"); got != (usageOf{calls: 1}) {
			t.Errorf("source charged %+v, want one call", got)
		}
		if got := loadUsage(counters, "b2"); got != (usageOf{}) {
			t.Errorf("destination charged %+v, want nothing", got)
		}
	})
	t.Run("write fails", func(t *testing.T) {
		t.Parallel()
		c, counters := newCopyRuntime(t)
		src := &readableBackend{payload: []byte("hello")}
		if _, err := c.StreamCopy(context.Background(), copyReq(ep("b1", src), ep("b2", writeRejectingBackend{}), 5)); err == nil {
			t.Fatal("StreamCopy succeeded, want the write failure")
		}
		if got := loadUsage(counters, "b1"); got != (usageOf{calls: 1, egress: 5}) {
			t.Errorf("source charged %+v, want one call and 5 bytes out", got)
		}
		if got := loadUsage(counters, "b2"); got != (usageOf{calls: 1}) {
			t.Errorf("destination charged %+v, want one call and no bytes", got)
		}
	})
}

// TestCore_StreamCopy_TransformReplacesTheBody verifies a transform sees the
// object read, its body and size are what the destination gets and what the
// destination is charged, the copy returns that size, and its release runs
// after the write.
func TestCore_StreamCopy_TransformReplacesTheBody(t *testing.T) {
	t.Parallel()
	c, counters := newCopyRuntime(t)
	src := &readableBackend{payload: []byte("hello")}
	dst := &recordingBackend{}
	var seen string
	released := 0
	req := copyReq(ep("b1", src), ep("b2", dst), 5)
	req.Transform = func(_ context.Context, read *backend.GetObjectResult) (io.Reader, int64, func(), error) {
		raw, err := io.ReadAll(read.Body)
		if err != nil {
			return nil, 0, nil, err
		}
		seen = string(raw)
		return strings.NewReader("HELLO!!"), 7, func() { released++ }, nil
	}

	written, err := c.StreamCopy(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamCopy: %v", err)
	}
	if seen != "hello" {
		t.Errorf("transform read %q, want the source bytes", seen)
	}
	if written != 7 || dst.putBodyLen != 7 {
		t.Errorf("written = %d, destination got %d, want 7 and 7", written, dst.putBodyLen)
	}
	if released != 1 {
		t.Errorf("release ran %d times, want once", released)
	}
	if got := loadUsage(counters, "b2"); got != (usageOf{calls: 1, ingress: 7}) {
		t.Errorf("destination charged %+v, want one call and the 7 transformed bytes", got)
	}
}

// TestCore_StreamCopy_TransformWithoutRelease verifies a transform that
// buffers nothing may return a nil release.
func TestCore_StreamCopy_TransformWithoutRelease(t *testing.T) {
	t.Parallel()
	c, _ := newCopyRuntime(t)
	req := copyReq(ep("b1", &readableBackend{payload: []byte("hello")}), ep("b2", &recordingBackend{}), 5)
	req.Transform = func(_ context.Context, read *backend.GetObjectResult) (io.Reader, int64, func(), error) {
		return read.Body, read.Size, nil, nil
	}
	if written, err := c.StreamCopy(context.Background(), req); err != nil || written != 5 {
		t.Fatalf("StreamCopy = %d, %v; want 5, nil", written, err)
	}
}

// TestCore_StreamCopy_TransformErrorTagsTransformPhase verifies a failed
// transform writes nothing and surfaces as a transform-phase copy error that
// still carries the transform's own error.
func TestCore_StreamCopy_TransformErrorTagsTransformPhase(t *testing.T) {
	t.Parallel()
	c, counters := newCopyRuntime(t)
	dst := &recordingBackend{}
	sentinel := errors.New("encode failed")
	req := copyReq(ep("b1", &readableBackend{payload: []byte("hello")}), ep("b2", dst), 5)
	req.Transform = func(context.Context, *backend.GetObjectResult) (io.Reader, int64, func(), error) {
		return nil, 0, nil, sentinel
	}

	_, err := c.StreamCopy(context.Background(), req)
	ce, ok := errors.AsType[*backend.CopyError](err)
	if !ok || ce.Phase != backend.CopyPhaseTransform {
		t.Fatalf("err = %v, want a transform-phase *backend.CopyError", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap the transform's error", err)
	}
	if dst.putBodyLen != 0 {
		t.Errorf("destination got %d bytes, want none", dst.putBodyLen)
	}
	if got := loadUsage(counters, "b2"); got != (usageOf{}) {
		t.Errorf("destination charged %+v, want nothing", got)
	}
}

func TestCore_Delete_ForwardsToBackend(t *testing.T) {
	t.Parallel()
	rb := &recordingBackend{}
	c := newTestCore(t)
	if err := c.Delete(context.Background(), "b1", rb, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
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
