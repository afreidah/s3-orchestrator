// -------------------------------------------------------------------------------
// Object Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts the object Manager pulls from *infra.BackendRuntime, the
// compression codec, and the detached-upload tracker. Pattern rationale:
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"io"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// Runtime is the subset of *infra.BackendRuntime the Manager and its read
// failover use. IsDraining backs the post-PUT drain-race re-check, since the
// EligibleForWrite filter ahead of it is racy.
type Runtime interface {
	RangeFetchRuntime
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	GetBackend(name string) (backend.ObjectBackend, error)
	IsDraining(name string) bool
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error)
	EligibleForWrite(ops []s3op.Operation, egress, ingress int64) []string
	ClassifyWriteError(span trace.Span, operation string, err error) error
	Quota() *counter.QuotaTracker
}

// RangeFetchRuntime is what a single ranged GET needs: the timed call plus the
// two meters it is charged against. Split out because a compressed read issues
// one per frame it touches, and the fetcher doing so needs nothing else.
type RangeFetchRuntime interface {
	GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error)
	Usage() *counter.UsageTracker
	Acct() *accounting.Recorder
}

// Codec is the compression surface the Manager uses: encode on write,
// decode on read.
type Codec interface {
	Compress(dst io.Writer, src io.Reader) (int64, error)
	DecompressRanged(ctx context.Context, f compression.RangeFetcher, compressedSize int64) (compression.RangedReader, error)
	InspectStored(ctx context.Context, f compression.RangeFetcher, storedSize int64) (int64, bool)
}

// DetachedRegistry is what the write path needs from the tracker of copies
// that outlive their response: a slot to run under, and the depth to log when
// there is none. Waiting for those copies at shutdown belongs to the runtime,
// so the manager cannot block on its own writes.
type DetachedRegistry interface {
	Begin() (release func(), admitted bool)
	Depth() int
}
