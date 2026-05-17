// -------------------------------------------------------------------------------
// Writepath Consumer-Declared Interface
//
// Author: Alex Freidah
//
// Narrow interface listing only the subset of *infra.Core behavior that
// the write Coordinator actually calls. Decouples the writepath
// subsystem from the wider proxy infrastructure surface so a method
// added to *infra.Core does not silently expand this consumer's
// dependency footprint.
//
// Mirrors the consumer-declared-interfaces pattern documented in
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
)

// WritepathCore is the subset of *infra.Core the Coordinator needs.
type WritepathCore interface {
	Backends() map[string]backend.ObjectBackend
	Usage() *counter.UsageTracker
	RoutingStrategy() config.RoutingStrategy
	EligibleForWrite(apiCalls, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error
	Log() *slog.Logger
}
