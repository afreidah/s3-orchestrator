// -------------------------------------------------------------------------------
// Writepath Consumer-Declared Interface
//
// Author: Alex Freidah
//
// Narrow contract the write Coordinator pulls from *infra.Core.
// Pattern rationale: docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package writepath

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
)

// WritepathCore is the subset of *infra.Core the Coordinator needs.
type WritepathCore interface {
	Backends() map[string]backend.ObjectBackend
	RoutingStrategy() config.RoutingStrategy
	EligibleForWrite(apiCalls, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error
	Acct() *accounting.Recorder
}
