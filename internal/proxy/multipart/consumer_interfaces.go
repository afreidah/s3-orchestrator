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
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
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
	Get(ctx context.Context, name string, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error)
	Put(ctx context.Context, op s3op.Operation, req *backend.PutRequest) (string, error)
	ClassifyWriteError(span trace.Span, op s3op.Operation, err error) error
	Acct() *accounting.Recorder
}
