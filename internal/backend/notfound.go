// -------------------------------------------------------------------------------
// Not-Found Error Classification
//
// Author: Alex Freidah
//
// IsNotFound recognises backend errors that wrap an HTTP 404 response.
// Lives here because every backend SDK we use (AWS, MinIO, GCS-via-S3,
// Azure-via-S3) returns errors that satisfy the HTTPStatusCode()
// interface, and several callers across the worker, write-path, and
// circuit-breaker layers need the same "object already absent" check.
// -------------------------------------------------------------------------------

package backend

import (
	"errors"
	"net/http"
)

// httpStatusError is the shape every backend SDK's response error shares:
// an error that also reports the HTTP status it was built from. Named so
// errors.AsType has an error-satisfying type to match against.
type httpStatusError interface {
	error
	HTTPStatusCode() int
}

// statusOf returns the HTTP status a response in err's chain carries, or 0
// when the chain holds no response.
func statusOf(err error) int {
	if respErr, ok := errors.AsType[httpStatusError](err); ok {
		return respErr.HTTPStatusCode()
	}
	return 0
}

// IsNotFound returns true if the error chain contains a typed response
// whose HTTPStatusCode() reports 404. A 404 from DELETE means the object
// is already absent on the backend, which is the desired end state -
// callers can treat it as idempotent success rather than as a failure
// worth retrying.
func IsNotFound(err error) bool {
	return statusOf(err) == http.StatusNotFound
}

// isRangeNotSatisfiable returns true if the error chain carries a 416: the
// object exists but is too short for the requested range.
func isRangeNotSatisfiable(err error) bool {
	return statusOf(err) == http.StatusRequestedRangeNotSatisfiable
}
