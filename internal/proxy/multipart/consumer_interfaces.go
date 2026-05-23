// -------------------------------------------------------------------------------
// Multipart Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts the multipart Manager pulls from *infra.Core and
// *writepath.Coordinator. Pattern rationale: docs/style-guide.md
// (Interface Design section).
// -------------------------------------------------------------------------------

package multipart

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
)

// MultipartCore is the subset of *infra.Core the multipart Manager needs.
type MultipartCore interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	Usage() *counter.UsageTracker // still needed for WithinLimits pre-flight checks; per-backend Record calls flow through Acct
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Acct() *accounting.Recorder
}
