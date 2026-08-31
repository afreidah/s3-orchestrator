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

import (
	"errors"
	"net/http"
)

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

	// ErrCopyHoldsOnlyDEK is returned when a copy cannot be removed because it
	// is the only one still carrying the key that decrypts the object while a
	// sibling copy has lost its encryption metadata. Removing it would make
	// the object permanently unreadable, so the caller skips the key and the
	// divergence is surfaced for repair.
	ErrCopyHoldsOnlyDEK = errors.New("copy holds the only usable encryption key for this object")

	// ErrEncryptionFlagMismatch means a row's encrypted flag disagrees with the
	// bytes stored for it: an envelope the row calls plaintext, or plaintext the
	// row calls an envelope. Callers must refuse to serve or hash such a copy
	// rather than guess which side is right.
	ErrEncryptionFlagMismatch = errors.New("stored bytes disagree with the object's encryption flag")

	// ErrObjectNotFound is returned when an object is not in the
	// location table.
	ErrObjectNotFound = &S3Error{
		StatusCode: http.StatusNotFound,
		Code:       "NoSuchKey",
		Message:    "object not found",
	}

	// ErrMultipartUploadNotFound is returned when a multipart upload
	// ID is not found.
	ErrMultipartUploadNotFound = &S3Error{
		StatusCode: http.StatusNotFound,
		Code:       "NoSuchUpload",
		Message:    "multipart upload not found",
	}

	// ErrInvalidRange is returned when a client's Range header cannot address
	// any byte of the object, such as a suffix range against a zero-length
	// object.
	ErrInvalidRange = &S3Error{
		StatusCode: http.StatusRequestedRangeNotSatisfiable,
		Code:       "InvalidRange",
		Message:    "the requested range is not satisfiable",
	}

	// ErrServiceUnavailable is returned to S3 clients when writes are
	// rejected during a database outage.
	ErrServiceUnavailable = &S3Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "ServiceUnavailable",
		Message:    "database unavailable, writes are temporarily rejected",
	}

	// ErrInsufficientStorage is returned when no backend has enough
	// quota at the manager-routing layer.
	ErrInsufficientStorage = &S3Error{
		StatusCode: http.StatusInsufficientStorage,
		Code:       "InsufficientStorage",
		Message:    "no backend has sufficient quota",
	}

	// ErrUsageLimitExceeded is returned when all backends holding an
	// object have exceeded their monthly usage limits.
	ErrUsageLimitExceeded = &S3Error{
		StatusCode: http.StatusTooManyRequests,
		Code:       "SlowDown",
		Message:    "monthly usage limit exceeded for all backends holding this object",
	}

	// ErrCleanupItemNotFound is returned by CleanupTxAdapter.GetCleanupQueueRow
	// when the row no longer exists - typically because another worker
	// already moved it to the DLQ or completed it. Callers treat this as
	// a benign no-op rather than an error.
	ErrCleanupItemNotFound = errors.New("cleanup queue row not found")
)

// -------------------------------------------------------------------------
// TAG VALIDATION ERRORS
// -------------------------------------------------------------------------

// Tag-set validation failures. These stay plain sentinels rather than
// S3Error values so the transport owns the mapping onto InvalidTag and
// BadRequest along with the message AWS words for each case; core wraps them
// with the offending measurement, which a generic S3Error message would drop.
var (
	// ErrTooManyTags is returned when a tag set exceeds MaxTagsPerObject.
	ErrTooManyTags = errors.New("too many tags for one object")

	// ErrEmptyTagKey is returned for a tag whose key is the empty string.
	// The key is half the primary key, so an empty one is not storable.
	ErrEmptyTagKey = errors.New("tag key must not be empty")

	// ErrTagKeyTooLong is returned when a key exceeds MaxTagKeyLength
	// UTF-16 code units.
	ErrTagKeyTooLong = errors.New("tag key too long")

	// ErrTagValueTooLong is returned when a value exceeds
	// MaxTagValueLength UTF-16 code units.
	ErrTagValueTooLong = errors.New("tag value too long")

	// ErrDuplicateTagKey is returned when a set names the same key twice.
	// Tag keys are case sensitive, so "a" and "A" are not duplicates.
	ErrDuplicateTagKey = errors.New("duplicate tag key")
)
