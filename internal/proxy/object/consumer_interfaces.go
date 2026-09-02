// -------------------------------------------------------------------------------
// Object Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts the object Manager pulls from *infra.BackendRuntime and
// *writepath.Coordinator. Pattern rationale: docs/style-guide.md
// (Interface Design section).
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"io"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// WriteRuntime is the subset of *infra.BackendRuntime the write-path Manager
// methods (put, copy, delete, mutation_finalize) reach for. IsDraining
// is here for the post-PUT drain-race re-check in attemptPutOnBackend
// (the upstream EligibleForWrite filter is racy; the re-check closes
// the window).
type WriteRuntime interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	IsDraining(name string) bool
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	EligibleForWrite(ops []s3op.Operation, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Acct() *accounting.Recorder
}

// Codec is the compression surface the Manager uses: encode on write,
// decode on read. A role rather than a single action, so it takes a role name;
// while it held Compress alone the -er form applied and it was Compressor.
//
// Declared rather than taking *compression.Codec because both halves fail on
// inputs the concrete codec cannot be made to produce - a mid-upload encode
// failure, a stored object that will not decode. A fake here is what lets
// those paths be tested at all.
type Codec interface {
	Compress(dst io.Writer, src io.Reader) (int64, error)
	DecompressRanged(ctx context.Context, f compression.RangeFetcher, compressedSize int64) (compression.RangedReader, error)
}

// -------------------------------------------------------------------------
// READ PATH
// -------------------------------------------------------------------------

// RangeFetchRuntime is what a single ranged GET needs: the timed call plus the
// two meters it is charged against. Split out because a compressed read issues
// one per frame it touches, and the fetcher doing so needs nothing else.
type RangeFetchRuntime interface {
	GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error)
	Usage() *counter.UsageTracker
	Acct() *accounting.Recorder
}

// ReadRuntime is the subset of *infra.BackendRuntime the read-path Manager
// methods (get, head, list, materialize) reach for. Usage() is still
// needed for WithinLimits pre-flight checks; per-backend Record calls
// flow through Acct.
type ReadRuntime interface {
	RangeFetchRuntime
	Backends() map[string]backend.ObjectBackend
	GetBackend(name string) (backend.ObjectBackend, error)
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error)
}

// Runtime composes the read and write role interfaces above into the
// single dependency object.Manager holds. *infra.BackendRuntime satisfies both
// implicitly, so production wiring stays one field; tests and future
// consumers may depend on the narrower role that matches their actual
// call surface. BackendOrder() is intentionally NOT included here — it
// is only used by *readpath.Failover, which receives its own
// readpath.ReadRuntime via Deps.BroadcastCore so the transitive requirement
// does not bleed into Runtime.
type Runtime interface {
	WriteRuntime
	ReadRuntime
}

// -------------------------------------------------------------------------
// WRITE PATH COLLABORATORS
// -------------------------------------------------------------------------

// WriteRouter is the routing subset of *writepath.Coordinator: pick a
// write-target backend given a size and an optional eligibility filter.
// Tests that exercise only routing decisions can mock this alone.
type WriteRouter interface {
	SelectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error)
	SelectWriteTarget(ctx context.Context, span trace.Span, operation s3op.Operation, size int64) (string, error)
}

// PendingWriter is the pending-intent subset of *writepath.Coordinator:
// insert the pre-PUT intent row and (on success) promote it into a
// permanent object_locations row. Tests that exercise only the
// pending-pattern handoff can mock this alone.
type PendingWriter interface {
	InsertPendingIntent(ctx context.Context, key, backendName string, size int64, form *core.StoredForm, id *core.ObjectIdentity) (string, error)
	RecordObjectAndPromoteIntent(ctx context.Context, span trace.Span, req *core.RecordObjectRequest) error
}

// CleanupWriter is the post-write commit + recovery subset of
// *writepath.Coordinator: commit the object row (with enqueue-on-failure
// fallback), recover from a record failure by deleting the orphaned
// bytes, or directly delete-or-enqueue a copy that should not survive.
// RecoverFromRecordFailure also backs the drain-race abort path in
// attemptPutOnBackend (when the post-PUT IsDraining re-check fires).
type CleanupWriter interface {
	RecordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, req *core.RecordObjectRequest) error
	RecoverFromRecordFailure(ctx context.Context, be backend.ObjectBackend, backendName, key, cleanupReason string, size int64)
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)
}

// Coordinator composes the three role interfaces above into the
// single dependency object.Manager holds. *writepath.Coordinator
// satisfies all three implicitly, so production wiring stays a single
// field, while tests and future consumers may depend on whichever
// narrower role matches their actual call surface.
type Coordinator interface {
	WriteRouter
	PendingWriter
	CleanupWriter
}
