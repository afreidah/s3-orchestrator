// -------------------------------------------------------------------------------
// Core Sentinel Errors
//
// Author: Alex Freidah
//
// Centralizes the error values returned by every store engine. S3Error carries
// the HTTP status code and S3 error code so the server layer can translate
// storage errors into S3 XML responses without per-handler error mapping.
// -------------------------------------------------------------------------------

package core

import "errors"

// -------------------------------------------------------------------------
// STRUCTURED ERROR
// -------------------------------------------------------------------------

// S3Error is a structured error that carries an HTTP status code and S3
// error code, allowing the server layer to translate storage errors into
// S3 XML responses without per-handler error mapping.
type S3Error struct {
	StatusCode int
	Code       string
	Message    string
}

// Error returns the human-readable error message.
func (e *S3Error) Error() string {
	return e.Message
}

// -------------------------------------------------------------------------
// SENTINEL ERRORS
// -------------------------------------------------------------------------

// ErrNoSpaceAvailable and related package-level variables used by this package.
var (
	// ErrNoSpaceAvailable is returned when no backend has sufficient quota.
	ErrNoSpaceAvailable = errors.New("no backend has sufficient quota")

	// ErrDBUnavailable is returned by CircuitBreakerStore when the
	// circuit is open. Manager uses errors.Is to trigger broadcast
	// fallback on reads or 503 rejection on writes.
	ErrDBUnavailable = errors.New("database unavailable")

	// ErrObjectNotFound is returned when an object is not in the
	// location table.
	ErrObjectNotFound = &S3Error{
		StatusCode: 404,
		Code:       "NoSuchKey",
		Message:    "object not found",
	}

	// ErrMultipartUploadNotFound is returned when a multipart upload
	// ID is not found.
	ErrMultipartUploadNotFound = &S3Error{
		StatusCode: 404,
		Code:       "NoSuchUpload",
		Message:    "multipart upload not found",
	}

	// ErrServiceUnavailable is returned to S3 clients when writes are
	// rejected during a database outage.
	ErrServiceUnavailable = &S3Error{
		StatusCode: 503,
		Code:       "ServiceUnavailable",
		Message:    "database unavailable, writes are temporarily rejected",
	}

	// ErrInsufficientStorage is returned when no backend has enough
	// quota at the manager-routing layer.
	ErrInsufficientStorage = &S3Error{
		StatusCode: 507,
		Code:       "InsufficientStorage",
		Message:    "no backend has sufficient quota",
	}

	// ErrUsageLimitExceeded is returned when all backends holding an
	// object have exceeded their monthly usage limits.
	ErrUsageLimitExceeded = &S3Error{
		StatusCode: 429,
		Code:       "SlowDown",
		Message:    "monthly usage limit exceeded for all backends holding this object",
	}

	// ErrCleanupItemNotFound is returned by CleanupTxAdapter.GetCleanupQueueRow
	// when the row no longer exists - typically because another worker
	// already moved it to the DLQ or completed it. Callers treat this as
	// a benign no-op rather than an error.
	ErrCleanupItemNotFound = errors.New("cleanup queue row not found")
)
