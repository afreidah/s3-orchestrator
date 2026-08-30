// -------------------------------------------------------------------------------
// Object Manager - Read Results
//
// Author: Alex Freidah
//
// The manager's own GET and HEAD result types. A backend result answers for the
// bytes a backend holds; anything the orchestrator derives from the metadata
// store instead has no place on it, because no backend ever sets it and the
// backend layer is meant to stay free of application concepts.
//
// The backend result is embedded rather than copied field by field, so callers
// keep reading ContentType, ETag and the rest straight off the result while the
// orchestrator-owned fields sit alongside them.
// -------------------------------------------------------------------------------

package object

import (
	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
)

// GetResult is a backend GET result plus the object-level metadata the
// orchestrator owns. TagCount is zero for an untagged object and also when the
// count could not be read, which the transport treats the same way: no
// tagging-count header.
type GetResult struct {
	*s3be.GetObjectResult
	TagCount int
}

// HeadResult is a backend HEAD result plus the object-level metadata the
// orchestrator owns. TagCount carries the same meaning as on GetResult.
type HeadResult struct {
	*s3be.HeadObjectResult
	TagCount int
}
