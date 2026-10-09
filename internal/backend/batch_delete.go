// -------------------------------------------------------------------------------
// Backend Batch Delete Capability
//
// Author: Alex Freidah
//
// Optional capability for backends that can remove many keys in one request.
// The orchestrator type-asserts to BatchDeleter to detect support, so a backend
// without it is unaffected and its keys are deleted one at a time. Not every
// S3-compatible provider implements multi-object delete, so a provider that
// answers NotImplemented is reported as ErrBatchDeleteNotSupported and the
// caller falls back the same way.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"net/http"

	"github.com/aws/smithy-go"
)

// maxBatchDeleteKeys is the most keys one S3 DeleteObjects request accepts.
const maxBatchDeleteKeys = 1000

// BatchDeleter is the optional capability for backends that delete many keys
// per request. DeleteObjects reports the keys it could not delete, keyed by
// key; a key absent from the map was deleted or already absent. A non-nil
// error means the request itself failed and nothing can be assumed deleted.
type BatchDeleter interface {
	DeleteObjects(ctx context.Context, keys []string) (failed map[string]error, err error)
}

var (
	_ BatchDeleter = (*S3Backend)(nil)
	_ BatchDeleter = (*CircuitBreakerBackend)(nil)
)

// ErrBatchDeleteNotSupported signals that a backend, or a decorator wrapping
// one, cannot delete keys in batches. Callers delete the keys one at a time.
var ErrBatchDeleteNotSupported = errors.New("backend does not support batch delete")

// batchKeyError is one key a batch delete could not remove, as the backend
// reported it. It carries a 404 for NoSuchKey so IsNotFound treats an already
// absent key the way it treats a single delete's 404.
type batchKeyError struct {
	code    string
	message string
}

// Error implements error.
func (e *batchKeyError) Error() string {
	return "batch delete failed for key: " + e.code + ": " + e.message
}

// HTTPStatusCode reports 404 for a key the backend says does not exist.
func (e *batchKeyError) HTTPStatusCode() int {
	if e.code == "NoSuchKey" {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// isNotImplemented reports whether err is a provider refusing the operation
// outright, by status or by S3 error code.
func isNotImplemented(err error) bool {
	if respErr, ok := errors.AsType[httpStatusError](err); ok && respErr.HTTPStatusCode() == http.StatusNotImplemented {
		return true
	}
	apiErr, ok := errors.AsType[smithy.APIError](err)
	return ok && apiErr.ErrorCode() == "NotImplemented"
}
