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
// type-check at this boundary.
type ObjectCore interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	GetBackend(name string) (backend.ObjectBackend, error)
	Usage() *counter.UsageTracker // still needed for WithinLimits pre-flight checks; per-backend Record calls flow through Acct
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	EligibleForWrite(apiCalls, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Acct() *accounting.Recorder
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
