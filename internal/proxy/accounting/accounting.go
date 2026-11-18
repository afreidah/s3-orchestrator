// -------------------------------------------------------------------------------
// Accounting Recorder - Per-Backend Usage and Operation Metric Helper
//
// Author: Alex Freidah
//
// Centralizes the per-backend accounting rules every storage subsystem
// must follow:
//
//   - Every backend call costs one API-call charge regardless of outcome
//     (success, error, usage-limit reject - the connection was made).
//   - A successful GET-like call adds egress bytes on top of the API
//     call charge. A successful PUT-like call adds ingress bytes.
//   - Every call is counted and timed on the per-operation metric.
//   - A call whose time does not depend on the object's size is timed for the
//     backend's pressure, and a backend under pressure is tried last.
//
// Every charge goes to the context's workload (audit.WithWorkload).
//
// Methods are named by intent (APICall, Ingress, Egress). The backend
// runtime's Get, Head, Put and Delete charge each call where it is sent; the
// recorder is called directly only for the egress a reader charges once it
// knows how many bytes it took. Held by every storage subsystem and worker
// through the consumer-declared Acct() contract.
// -------------------------------------------------------------------------------

package accounting

import (
	"context"
	"slices"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// OperationRecordFunc is the per-operation metric callback. Wraps the
// existing Prometheus operation histogram so the Recorder does not have
// to import the metrics package directly.
type OperationRecordFunc func(operation, backend string, start time.Time, err error)

// Recorder centralizes the per-backend accounting rules. Safe for
// concurrent use: all state lives on the underlying trackers and
// OperationRecordFunc, which are themselves safe.
type Recorder struct {
	usage    *counter.UsageTracker
	pressure *counter.PressureTracker
	recordOp OperationRecordFunc
}

// New constructs a Recorder. usage credits the per-backend API call
// and ingress/egress byte counters; pressure times calls and says which
// backends are under pressure, and is nil where nothing judges it; recordOp
// emits the per-operation Prometheus histogram observation.
func New(usage *counter.UsageTracker, pressure *counter.PressureTracker, recordOp OperationRecordFunc) *Recorder {
	return &Recorder{usage: usage, pressure: pressure, recordOp: recordOp}
}

// -------------------------------------------------------------------------
// ADMISSION
// -------------------------------------------------------------------------

// Allow reports whether the backend can absorb one call of op within its
// monthly API-call, egress and ingress limits.
func (r *Recorder) Allow(backend string, op s3op.Operation, egress, ingress int64) bool {
	return r.usage.WithinLimits(backend, op, egress, ingress)
}

// -------------------------------------------------------------------------
// API-CALL CHARGES
// -------------------------------------------------------------------------

// APICall charges one call of op against the backend's usage counter. Call it
// after every attempt that reached the backend, whatever the outcome.
func (r *Recorder) APICall(ctx context.Context, op s3op.Operation, backend string) {
	r.usage.Record(ctx, backend, op, 0, 0)
}

// -------------------------------------------------------------------------
// SUCCESSFUL TRANSFERS
// -------------------------------------------------------------------------

// Egress credits one API call + sizeBytes of egress on the backend.
// Use after a successful GET-like operation (the bytes left the
// backend toward the client).
func (r *Recorder) Egress(ctx context.Context, op s3op.Operation, backend string, sizeBytes int64) {
	r.usage.Record(ctx, backend, op, sizeBytes, 0)
}

// Ingress credits one API call + sizeBytes of ingress on the backend.
// Use after a successful PUT-like operation (the bytes arrived at the
// backend from the client).
func (r *Recorder) Ingress(ctx context.Context, op s3op.Operation, backend string, sizeBytes int64) {
	r.usage.Record(ctx, backend, op, 0, sizeBytes)
}

// -------------------------------------------------------------------------
// OPERATION METRIC
// -------------------------------------------------------------------------

// Operation records the per-operation latency histogram. It is separate from
// APICall so a failure path can charge the call without a success observation.
func (r *Recorder) Operation(op s3op.Operation, backend string, start time.Time, err error) {
	r.recordOp(op.String(), backend, start, err)
}

// -------------------------------------------------------------------------
// PRESSURE
// -------------------------------------------------------------------------

// Latency records how long one call to backend took, with timedOut set when
// the backend timeout ended it.
func (r *Recorder) Latency(backend string, elapsed time.Duration, timedOut bool) {
	if r.pressure != nil {
		r.pressure.Observe(backend, elapsed, timedOut)
	}
}

// Pressure returns the reasons backend is under pressure, empty when it is
// not or nothing judges it.
func (r *Recorder) Pressure(backend string) counter.Pressure {
	if r.pressure == nil {
		return 0
	}
	return r.pressure.Pressure(backend)
}

// PressuredLast returns items with the ones on a backend under pressure moved
// to the end, otherwise in their order. items itself comes back when no
// backend among them is under pressure.
func PressuredLast[T any](r *Recorder, items []T, backend func(T) string) []T {
	rank := func(item T) int {
		if r.Pressure(backend(item)) != 0 {
			return 1
		}
		return 0
	}
	if !slices.ContainsFunc(items, func(item T) bool { return rank(item) != 0 }) {
		return items
	}
	ordered := slices.Clone(items)
	slices.SortStableFunc(ordered, func(a, b T) int { return rank(a) - rank(b) })
	return ordered
}
