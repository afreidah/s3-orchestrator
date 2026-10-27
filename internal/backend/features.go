// -------------------------------------------------------------------------------
// Backend - Unsupported Features
//
// Author: Alex Freidah
//
// What a backend has found it does not support, learned from the first
// refusal. Range, multi-object delete and server-side copy each have a
// fallback, and a backend that has refused one is not asked again: the call
// answers with the feature's unsupported error and the caller takes the
// fallback without a request. The first refusal of each feature logs one
// warning and sets s3o_backend_unsupported.
//
// The set lives as long as the backend, which is built once at startup, so a
// provider that gains a feature is picked up on the next restart.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/smithy-go"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ErrUnsupported matches every error that says a backend lacks an optional
// feature. Callers take the feature's fallback, and a circuit breaker does not
// count it, since a missing feature is not a fault.
var ErrUnsupported = errors.New("backend does not support it")

// unsupportedFeatures reads the features a backend has found unsupported.
// *S3Backend and *CircuitBreakerBackend implement it.
type unsupportedFeatures interface {
	Unsupported() core.BackendFeatures
}

// Unsupported returns the features b has found unsupported. A backend that
// learns nothing reports none.
func Unsupported(b ObjectBackend) core.BackendFeatures {
	if u, ok := b.(unsupportedFeatures); ok {
		return u.Unsupported()
	}
	return 0
}

// Unsupported returns the features the backend has refused since it was built.
func (b *S3Backend) Unsupported() core.BackendFeatures {
	return core.BackendFeatures(b.unsupported.Load())
}

// lacks reports whether the backend has already refused feature.
func (b *S3Backend) lacks(feature core.BackendFeatures) bool {
	return b.Unsupported().Has(feature)
}

// markUnsupported records that the backend refused feature. Only the call that
// first records it warns and sets the gauge, so a busy backend logs once.
func (b *S3Backend) markUnsupported(ctx context.Context, feature core.BackendFeatures, err error) {
	if core.BackendFeatures(b.unsupported.Or(uint64(feature))).Has(feature) {
		return
	}
	telemetry.BackendUnsupported.WithLabelValues(b.name, feature.String()).Set(1)
	slog.Default().With(logfmt.Component("backend")).WarnContext(ctx,
		"backend does not support a feature; using the fallback until restart",
		"backend", b.name, "feature", feature.String(), logfmt.Err(err))
}

// isUnsupported reports whether err is a provider refusing the request itself
// rather than failing to carry it out: not implemented, or method not allowed.
// Sending the same request again would be refused the same way.
func isUnsupported(err error) bool {
	if respErr, ok := errors.AsType[httpStatusError](err); ok {
		switch respErr.HTTPStatusCode() {
		case http.StatusNotImplemented, http.StatusMethodNotAllowed:
			return true
		}
	}
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiErr.ErrorCode() {
		case "NotImplemented", "MethodNotAllowed":
			return true
		}
	}
	return false
}
