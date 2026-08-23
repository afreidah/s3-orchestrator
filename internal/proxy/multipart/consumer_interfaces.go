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

// MultipartCodec is the compression surface assembly uses. Both halves are
// needed for one write: the assembled stream is encoded to learn what it costs,
// and decoded back out of that same buffer when the encoding did not earn its
// place, which is the only way to recover a plaintext the part pipe has already
// delivered once.
//
// Declared rather than taking *compression.Codec because a mid-assembly encode
// failure is a path worth testing and the concrete codec cannot be made to
// produce one.
type MultipartCodec interface {
	Compress(dst io.Writer, src io.Reader) (int64, error)
	Decompress(rs io.ReadSeeker) (io.ReadCloser, error)
}

// MultipartRuntime is the subset of *infra.BackendRuntime the multipart Manager needs.
type MultipartRuntime interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	Usage() *counter.UsageTracker // still needed for WithinLimits pre-flight checks; per-backend Record calls flow through Acct
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Acct() *accounting.Recorder
}
