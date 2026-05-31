// -------------------------------------------------------------------------------
// Object Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts the object Manager pulls from *infra.Core and
// *writepath.Coordinator. Pattern rationale: docs/style-guide.md
// (Interface Design section).
// -------------------------------------------------------------------------------

package object

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ObjectCore is the subset of *infra.Core the object Manager needs.
// BackendOrder is here for *readpath.Failover, which Manager constructs
// from its own ObjectCore; the transitive requirement satisfies its
// type-check at this boundary. IsDraining is here for the post-PUT
// drain-race re-check in attemptPutOnBackend (the upstream
// EligibleForWrite filter is racy; the re-check closes the window).
type ObjectCore interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	GetBackend(name string) (backend.ObjectBackend, error)
	Usage() *counter.UsageTracker // still needed for WithinLimits pre-flight checks; per-backend Record calls flow through Acct
	IsDraining(name string) bool
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	EligibleForWrite(apiCalls, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Acct() *accounting.Recorder
}

// WriteRouter is the routing subset of *writepath.Coordinator: pick a
// write-target backend given a size and an optional eligibility filter.
// Tests that exercise only routing decisions can mock this alone.
type WriteRouter interface {
	SelectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error)
	SelectWriteTarget(ctx context.Context, span trace.Span, operation string, size int64) (string, error)
}

// PendingWriter is the pending-intent subset of *writepath.Coordinator:
// insert the pre-PUT intent row and (on success) promote it into a
// permanent object_locations row. Tests that exercise only the
// pending-pattern handoff can mock this alone.
type PendingWriter interface {
	InsertPendingIntent(ctx context.Context, key, backendName string, size int64, enc *core.EncryptionMeta) (string, error)
	RecordObjectAndPromoteIntent(ctx context.Context, span trace.Span, key, backendName string, size int64, enc *core.EncryptionMeta, intentID string) error
}

// CleanupWriter is the post-write commit + recovery subset of
// *writepath.Coordinator: commit the object row (with enqueue-on-failure
// fallback), recover from a record failure by deleting the orphaned
// bytes, or directly delete-or-enqueue a copy that should not survive.
// RecoverFromRecordFailure also backs the drain-race abort path in
// attemptPutOnBackend (when the post-PUT IsDraining re-check fires).
type CleanupWriter interface {
	RecordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, key, backendName string, size int64, enc *core.EncryptionMeta) error
	RecoverFromRecordFailure(ctx context.Context, be backend.ObjectBackend, backendName, key, cleanupReason string, size int64)
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)
}

// ObjectCoordinator composes the three role interfaces above into the
// single dependency object.Manager holds. *writepath.Coordinator
// satisfies all three implicitly, so production wiring stays a single
// field, while tests and future consumers may depend on whichever
// narrower role matches their actual call surface.
type ObjectCoordinator interface {
	WriteRouter
	PendingWriter
	CleanupWriter
}
