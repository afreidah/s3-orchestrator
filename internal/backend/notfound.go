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

import "errors"

// IsNotFound returns true if the error chain contains a typed response
// whose HTTPStatusCode() reports 404. A 404 from DELETE means the object
// is already absent on the backend, which is the desired end state -
// callers can treat it as idempotent success rather than as a failure
// worth retrying.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var respErr interface{ HTTPStatusCode() int }
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404
}
