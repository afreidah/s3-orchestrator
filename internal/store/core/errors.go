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

// The sentinel errors the store layer returns. Each message states the
// condition; what follows is the handling a caller owes
// the ones that are not simply propagated.
//
// ErrDBUnavailable is matched with errors.Is to pick the degraded behaviour for
// an open circuit: broadcast fallback on reads, 503 on writes.
//
// ErrCopyHoldsOnlyDEK and ErrEncryptionFlagMismatch both mean the copies of one
// object disagree about how they are stored. Neither is recoverable by guessing
// which side is right, so callers skip the object and surface the divergence for
// repair rather than serving, hashing or deleting on a coin flip.
//
// ErrCleanupItemNotFound is benign: another worker completed the row or moved it
// to the DLQ first, so the caller treats it as a no-op.
var (
	ErrNoSpaceAvailable       = errors.New("no backend has sufficient quota")
	ErrDBUnavailable          = errors.New("database unavailable")
	ErrCopyHoldsOnlyDEK       = errors.New("copy holds the only usable encryption key for this object")
	ErrEncryptionFlagMismatch = errors.New("stored bytes disagree with the object's encryption flag")
	ErrCleanupItemNotFound    = errors.New("cleanup queue row not found")
	ErrNoCopiesToRecord       = errors.New("record object request names no copies")

	ErrObjectNotFound = &S3Error{
		StatusCode: http.StatusNotFound,
		Code:       "NoSuchKey",
		Message:    "object not found",
	}

	ErrMultipartUploadNotFound = &S3Error{
		StatusCode: http.StatusNotFound,
		Code:       "NoSuchUpload",
		Message:    "multipart upload not found",
	}

	// Raised for a range that cannot address any byte of the object, such as a
	// suffix range against a zero-length one.
	ErrInvalidRange = &S3Error{
		StatusCode: http.StatusRequestedRangeNotSatisfiable,
		Code:       "InvalidRange",
		Message:    "the requested range is not satisfiable",
	}

	ErrServiceUnavailable = &S3Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "ServiceUnavailable",
		Message:    "database unavailable, writes are temporarily rejected",
	}

	ErrInsufficientStorage = &S3Error{
		StatusCode: http.StatusInsufficientStorage,
		Code:       "InsufficientStorage",
		Message:    "no backend has sufficient quota",
	}

	ErrUsageLimitExceeded = &S3Error{
		StatusCode: http.StatusTooManyRequests,
		Code:       "SlowDown",
		Message:    "monthly usage limit exceeded for all backends holding this object",
	}
)

// -------------------------------------------------------------------------
// TAG VALIDATION ERRORS
// -------------------------------------------------------------------------

// Tag-set validation failures. These stay plain sentinels rather than
// S3Error values so the transport owns the mapping onto InvalidTag and
// BadRequest along with the message AWS words for each case; core wraps them
// with the offending measurement, which a generic S3Error message would drop.
//
// An empty key is unstorable rather than merely invalid: the key is half the
// primary key. Duplicate detection is case sensitive, so "a" and "A" are two
// keys. Lengths are measured in UTF-16 code units, as AWS measures them.
var (
	ErrTooManyTags     = errors.New("too many tags for one object")
	ErrEmptyTagKey     = errors.New("tag key must not be empty")
	ErrTagKeyTooLong   = errors.New("tag key too long")
	ErrTagValueTooLong = errors.New("tag value too long")
	ErrDuplicateTagKey = errors.New("duplicate tag key")
)
