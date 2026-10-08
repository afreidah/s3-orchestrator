// -------------------------------------------------------------------------------
// Multipart Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts the multipart Manager pulls from *infra.BackendRuntime and
// *writepath.Coordinator. Pattern rationale: docs/style-guide.md
// (Interface Design section).
// -------------------------------------------------------------------------------

package multipart

import (
	"context"
	"io"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
)

// Codec is the compression surface assembly uses: the assembled stream is
// encoded, then decoded back out of the same buffer when the encoding misses
// min_ratio, because the part pipe delivers the plaintext only once.
type Codec interface {
	Compress(dst io.Writer, src io.Reader) (int64, error)
	Decompress(rs io.ReadSeeker) (io.ReadCloser, error)
}

// Runtime is the subset of *infra.BackendRuntime the multipart Manager needs.
type Runtime interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	Usage() *counter.UsageTracker // still needed for WithinLimits pre-flight checks; per-backend Record calls flow through Acct
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Acct() *accounting.Recorder
}
