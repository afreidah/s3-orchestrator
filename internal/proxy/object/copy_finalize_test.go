// -------------------------------------------------------------------------------
// Object Manager - Copy Finalizer Tests
//
// Author: Alex Freidah
//
// finalizeCopy closes out both copy paths. A server-side copy is charged as
// one API call and marks its span; a materialized copy also charges the bytes
// that crossed the orchestrator, egress on the source and ingress on the
// destination. A record failure leaves both the counters and the location
// hint alone.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// copyDest is the destination key, and copySize the bytes each copy moves.
const (
	copyDest = "dst"
	copySize = 7
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// finalizeCopyFixture runs finalizeCopy from src to dst over store, with the
// destination's location hint set beforehand, and returns the fleet, the
// recorded span and the call's result.
func finalizeCopyFixture(t *testing.T, store storetest.MetadataStore, native bool) (*fleet, sdktrace.ReadOnlySpan, string, error) {
	t.Helper()
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{
		"src": backendtest.NewInMemory(), copyDest: backendtest.NewInMemory(),
	}, &fleetOpts{Order: []string{"src", copyDest}})
	mgr.LocationCache().Set(copyDest, "stale")

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	_, span := tp.Tracer("test").Start(context.Background(), "copy")
	etag, err := mgr.finalizeCopy(context.Background(), &copyContext{
		span:            span,
		sourceKey:       "source",
		srcBackendName:  "src",
		destKey:         copyDest,
		destStorageKey:  copyDest + "!w1",
		destBackendName: copyDest,
		size:            copySize,
		identity:        &core.ObjectIdentity{ETag: `"src-etag"`},
		start:           time.Now(),
	}, `"backend-etag"`, native)
	span.End()
	return mgr, sr.Ended()[0], etag, err
}

// usageOf reads one usage counter for a backend.
func usageOf(mgr *fleet, name, field string) int64 {
	return mgr.Runtime.Usage().Backend().Load(name, field)
}

// nativeCopyAttr reports whether the span was marked as a server-side copy.
func nativeCopyAttr(span sdktrace.ReadOnlySpan) bool {
	for _, kv := range span.Attributes() {
		if kv.Key == telemetry.AttrNativeCopy && kv.Value.Type() == attribute.BOOL {
			return kv.Value.AsBool()
		}
	}
	return false
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestFinalizeCopy_Native verifies a server-side copy answers with the
// source's ETag, marks its span, charges no transfer bytes, and drops the
// destination's location hint.
func TestFinalizeCopy_Native(t *testing.T) {
	t.Parallel()
	store, calls := putObjectStore(t)
	mgr, span, etag, err := finalizeCopyFixture(t, store, true)
	if err != nil {
		t.Fatalf("finalizeCopy: %v", err)
	}
	if etag != `"src-etag"` {
		t.Errorf("etag = %q, want the source's", etag)
	}
	if len(calls.recordObject) != 1 || calls.recordObject[0].Key != copyDest {
		t.Errorf("records = %+v, want one for %s", calls.recordObject, copyDest)
	}
	if !nativeCopyAttr(span) {
		t.Error("span not marked as a native copy")
	}
	if got := usageOf(mgr, "src", counter.FieldEgressBytes); got != 0 {
		t.Errorf("source egress = %d, want 0", got)
	}
	if got := usageOf(mgr, copyDest, counter.FieldIngressBytes); got != 0 {
		t.Errorf("destination ingress = %d, want 0", got)
	}
	if _, ok := mgr.LocationCache().Get(copyDest); ok {
		t.Error("destination location hint survived the copy")
	}
}

// TestFinalizeCopy_Materialized verifies a stream-through copy charges egress
// on the source and ingress on the destination, and leaves the span unmarked.
func TestFinalizeCopy_Materialized(t *testing.T) {
	t.Parallel()
	store, _ := putObjectStore(t)
	mgr, span, _, err := finalizeCopyFixture(t, store, false)
	if err != nil {
		t.Fatalf("finalizeCopy: %v", err)
	}
	if nativeCopyAttr(span) {
		t.Error("materialized copy marked as native")
	}
	if got := usageOf(mgr, "src", counter.FieldEgressBytes); got != copySize {
		t.Errorf("source egress = %d, want %d", got, copySize)
	}
	if got := usageOf(mgr, copyDest, counter.FieldIngressBytes); got != copySize {
		t.Errorf("destination ingress = %d, want %d", got, copySize)
	}
	if _, ok := mgr.LocationCache().Get(copyDest); ok {
		t.Error("destination location hint survived the copy")
	}
}

// TestFinalizeCopy_RecordFailure verifies a failed record surfaces for both
// paths without charging transfer bytes or dropping the location hint.
func TestFinalizeCopy_RecordFailure(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{true, false} {
		c := &objectsCalls{}
		store := storetest.NewMockMetadataStore(gomock.NewController(t))
		store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
			DoAndReturn(stubObjRecord(c, errors.New("commit failed"))).AnyTimes()
		storetest.Permissive(store)

		mgr, span, etag, err := finalizeCopyFixture(t, store, native)
		if err == nil || etag != "" {
			t.Errorf("native=%v: finalizeCopy = %q, %v; want an error", native, etag, err)
		}
		if nativeCopyAttr(span) {
			t.Errorf("native=%v: span marked native despite the failure", native)
		}
		if got := usageOf(mgr, "src", counter.FieldEgressBytes); got != 0 {
			t.Errorf("native=%v: source egress = %d, want 0", native, got)
		}
		if _, ok := mgr.LocationCache().Get(copyDest); !ok {
			t.Errorf("native=%v: a failed copy dropped the location hint", native)
		}
	}
}
