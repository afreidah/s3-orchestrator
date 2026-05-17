// -------------------------------------------------------------------------------
// Multipart Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow interfaces listing only the subset of *infra.Core and
// *writepath.Coordinator behavior that the multipart Manager actually
// calls. Decouples the multipart subsystem from the wider proxy
// infrastructure surface so a method added to *infra.Core or
// *writepath.Coordinator does not silently expand this consumer's
// dependency footprint, and so future tests can mock at the granularity
// of what is used.
//
// Mirrors the consumer-declared-interfaces pattern documented in
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package multipart

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// MultipartCore is the subset of *infra.Core the multipart Manager
// needs. Log is intentionally absent: the logger is observability
// infrastructure owned by the Manager (built via
// logfmt.Component("multipart") in New), not a behavior dependency.
type MultipartCore interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	Usage() *counter.UsageTracker // still needed for WithinLimits pre-flight checks; per-backend Record calls flow through Acct
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Acct() *accounting.Recorder
}

// MultipartCoordinator is the subset of *writepath.Coordinator the
// multipart Manager needs.
type MultipartCoordinator interface {
	SelectWriteTarget(ctx context.Context, span trace.Span, operation string, size int64) (string, error)
	RecoverFromRecordFailure(ctx context.Context, be backend.ObjectBackend, backendName, key, cleanupReason string, size int64)
	RecordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, key, backendName string, size int64, enc *core.EncryptionMeta) error
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)
}
