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
//   - The per-operation metric (RecordOperation) carries the
//     operation/backend/start/err tuple for the Prometheus histogram.
//
// Methods are named by intent (APICall, Ingress, Egress) and a few
// combo helpers (PutSuccess, GetSuccess, OperationFailed) bundle the
// Operation + bytes pair so the matching pair cannot drift. Held by
// every storage subsystem and worker through the consumer-declared
// Acct() contract.
// -------------------------------------------------------------------------------

package accounting

import (
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
)

// OperationRecordFunc is the per-operation metric callback. Wraps the
// existing Prometheus operation histogram so the Recorder does not have
// to import the metrics package directly.
type OperationRecordFunc func(operation, backend string, start time.Time, err error)

// Recorder centralizes the per-backend accounting rules. Safe for
// concurrent use: all state lives on the underlying UsageTracker and
// OperationRecordFunc, which are themselves safe.
type Recorder struct {
	usage    *counter.UsageTracker
	recordOp OperationRecordFunc
}

// New constructs a Recorder. usage credits the per-backend API call
// and ingress/egress byte counters; recordOp emits the per-operation
// Prometheus histogram observation.
func New(usage *counter.UsageTracker, recordOp OperationRecordFunc) *Recorder {
	return &Recorder{usage: usage, recordOp: recordOp}
}

// -------------------------------------------------------------------------
// API-CALL CHARGES
// -------------------------------------------------------------------------

// APICall credits one API call against the backend's usage counter.
// Call after every attempt that contacted the backend, regardless of
// outcome: the HTTP call was made either way.
func (r *Recorder) APICall(backend string) {
	r.usage.Record(backend, 1, 0, 0)
}

// APICalls credits n API calls against the backend's usage counter.
// For paginated lists, multipart completes that touch many part
// objects, and other multi-call operations.
func (r *Recorder) APICalls(backend string, n int64) {
	r.usage.Record(backend, n, 0, 0)
}

// -------------------------------------------------------------------------
// SUCCESSFUL TRANSFERS
// -------------------------------------------------------------------------

// Egress credits one API call + sizeBytes of egress on the backend.
// Use after a successful GET-like operation (the bytes left the
// backend toward the client).
func (r *Recorder) Egress(backend string, sizeBytes int64) {
	r.usage.Record(backend, 1, sizeBytes, 0)
}

// Ingress credits one API call + sizeBytes of ingress on the backend.
// Use after a successful PUT-like operation (the bytes arrived at the
// backend from the client).
func (r *Recorder) Ingress(backend string, sizeBytes int64) {
	r.usage.Record(backend, 1, 0, sizeBytes)
}

// -------------------------------------------------------------------------
// OPERATION METRIC
// -------------------------------------------------------------------------

// Operation emits the per-operation Prometheus histogram observation.
// Pair with APICall / Egress / Ingress for the usage-counter side; the
// two surfaces are deliberately separate so callers can record the
// API-call charge on failure paths without also emitting a fake
// success observation.
func (r *Recorder) Operation(operation, backend string, start time.Time, err error) {
	r.recordOp(operation, backend, start, err)
}

// -------------------------------------------------------------------------
// COMBOS
// -------------------------------------------------------------------------

// PutSuccess is the canonical "PUT succeeded" pair: emit the per-op
// metric with a nil error and credit one API call + ingress bytes.
func (r *Recorder) PutSuccess(operation, backend string, sizeBytes int64, start time.Time) {
	r.recordOp(operation, backend, start, nil)
	r.usage.Record(backend, 1, 0, sizeBytes)
}

// GetSuccess is the canonical "GET succeeded" pair: emit the per-op
// metric with a nil error and credit one API call + egress bytes.
func (r *Recorder) GetSuccess(operation, backend string, sizeBytes int64, start time.Time) {
	r.recordOp(operation, backend, start, nil)
	r.usage.Record(backend, 1, sizeBytes, 0)
}

// OperationFailed is the canonical "the call reached the backend and
// failed" pair: emit the per-op metric with the err and credit one API
// call (no bytes, since nothing transferred).
func (r *Recorder) OperationFailed(operation, backend string, start time.Time, err error) {
	r.recordOp(operation, backend, start, err)
	r.usage.Record(backend, 1, 0, 0)
}
