// -------------------------------------------------------------------------------
// Writepath Consumer-Declared Interface
//
// Author: Alex Freidah
//
// Narrow contract the write Coordinator pulls from *infra.BackendRuntime.
// Pattern rationale: docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package writepath

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// WriteRuntime is the subset of *infra.BackendRuntime the Coordinator needs.
type WriteRuntime interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	RoutingStrategy() config.RoutingStrategy
	Quota() *counter.QuotaTracker
	EligibleForWrite(op s3op.Operation, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	DeleteMany(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error
	StreamCopy(ctx context.Context, req *backend.CopyRequest) (int64, error)
	Acct() *accounting.Recorder
}
