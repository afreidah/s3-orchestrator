// -------------------------------------------------------------------------------
// Object Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow interfaces listing only the subset of *infra.Core and
// *writepath.Coordinator behavior that the object Manager actually
// calls. Decouples the object subsystem from the wider proxy
// infrastructure surface so a method added to *infra.Core or
// *writepath.Coordinator does not silently expand this consumer's
// dependency footprint, and so future tests can mock at the granularity
// of what is used.
//
// Mirrors the consumer-declared-interfaces pattern documented in
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ObjectCore is the subset of *infra.Core the object Manager needs.
// Log is intentionally absent: the logger is observability
// infrastructure owned by the Manager (built via
// logfmt.Component("object") in New), not a behavior dependency.
//
// BackendOrder is on this interface even though no method in the
// object package calls it directly: the read-failover orchestrator
// owned by Manager (*readpath.Failover) needs it, and Manager passes
// its own ObjectCore through to readpath.New. The transitive
// requirement still lives at this boundary because that is where the
// type-check has to be satisfied.
type ObjectCore interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	GetBackend(name string) (backend.ObjectBackend, error)
	Usage() *counter.UsageTracker
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	EligibleForWrite(apiCalls, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	RecordOperation(operation, backend string, start time.Time, err error)
}

// ObjectCoordinator is the subset of *writepath.Coordinator the object
// Manager needs.
type ObjectCoordinator interface {
	SelectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error)
	SelectWriteTarget(ctx context.Context, span trace.Span, operation string, size int64) (string, error)
	InsertPendingIntent(ctx context.Context, key, backendName string, size int64, enc *core.EncryptionMeta) (string, error)
	RecordObjectAndPromoteIntent(ctx context.Context, span trace.Span, key, backendName string, size int64, enc *core.EncryptionMeta, intentID string) error
	RecordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, key, backendName string, size int64, enc *core.EncryptionMeta) error
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)
}
