// -------------------------------------------------------------------------------
// Multipart Completion Manifest Validation
//
// Author: Alex Freidah
//
// Validates the part list a client sends to CompleteMultipartUpload against
// the S3 protocol invariants and against what was actually stored. Every check
// here runs before assembly starts and before any cleanup is scheduled, so a
// rejected manifest leaves the upload exactly as it was and the client can fix
// the request and retry.
//
// The checks are ordered cheapest-first: shape (range, ordering, duplicates,
// count) needs no store round-trip, so a malformed manifest is rejected before
// the parts are read.
// -------------------------------------------------------------------------------

package multipart

import (
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// MinPartNumber and the other manifest bounds, all of them S3's rather than
// this implementation's. A manifest outside them is rejected here rather than
// passed to a backend, so a client that splits too finely fails on the same
// rule every other S3 implementation would have applied. MaxPartCount equals
// MaxPartNumber because part numbers are unique and in range.
const (
	MinPartNumber = 1
	MaxPartNumber = core.MaxPartNumber
	MaxPartCount  = MaxPartNumber

	MinPartSizeBytes = 5 * 1024 * 1024 // every part except the last
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// validateManifestShape checks everything that can be known without reading
// the stored parts: part numbers in range, strictly ascending, no duplicates,
// and no more than MaxPartCount entries. Strictly ascending subsumes the
// duplicate check, but both errors are distinct in S3 so they are reported
// separately.
func validateManifestShape(manifest []core.CompletePart) error {
	if len(manifest) == 0 {
		return &core.S3Error{
			StatusCode: http.StatusBadRequest,
			Code:       "InvalidRequest",
			Message:    "You must specify at least one part",
		}
	}
	if len(manifest) > MaxPartCount {
		return &core.S3Error{
			StatusCode: http.StatusBadRequest,
			Code:       "InvalidRequest",
			Message:    fmt.Sprintf("Too many parts: %d exceeds the maximum of %d", len(manifest), MaxPartCount),
		}
	}

	for i, p := range manifest {
		if p.PartNumber < MinPartNumber || p.PartNumber > MaxPartNumber {
			return &core.S3Error{
				StatusCode: http.StatusBadRequest,
				Code:       "InvalidPart",
				Message: fmt.Sprintf("Part number %d is outside the valid range %d-%d",
					p.PartNumber, MinPartNumber, MaxPartNumber),
			}
		}
		if i == 0 {
			continue
		}
		prev := manifest[i-1].PartNumber
		if p.PartNumber == prev {
			return &core.S3Error{
				StatusCode: http.StatusBadRequest,
				Code:       "InvalidPart",
				Message:    fmt.Sprintf("Duplicate part number %d", p.PartNumber),
			}
		}
		if p.PartNumber < prev {
			return &core.S3Error{
				StatusCode: http.StatusBadRequest,
				Code:       "InvalidPartOrder",
				Message:    "The list of parts was not in ascending order",
			}
		}
	}
	return nil
}

// clientPartETag returns the ETag UploadPart handed the client for this part:
// the MD5 of the bytes they sent. Parts uploaded before that digest was
// recorded fall back to the backend's value, which is what those clients were
// given and therefore what they will send back.
func clientPartETag(p *core.MultipartPart) string {
	if p.PlaintextETag != "" {
		return etag.Single(p.PlaintextETag)
	}
	return p.ETag
}

// presentToClient rewrites a stored part into what UploadPart told the client:
// the digest of the bytes it sent and their size. An encrypted part is stored
// as a larger envelope, and a client resuming an upload sums the listed sizes
// to find its offset, so the envelope size would send it past the end of what
// it has written.
func presentToClient(p *core.MultipartPart) {
	p.ETag = clientPartETag(p)
	p.SizeBytes = p.ClientSize(p.SizeBytes)
}

// validateManifestAgainstStored compares the manifest to the parts actually
// held for the upload: each part's ETag must match, and every part but the
// last must meet the minimum size. stored holds the manifest's parts in its
// order, which collectRequestedParts guarantees.
//
// enforceMinSize lets an operator turn off the 5 MiB floor for deployments
// whose existing writers upload smaller parts.
func validateManifestAgainstStored(manifest []core.CompletePart, stored []core.MultipartPart, enforceMinSize bool) error {
	for i, want := range manifest {
		got := &stored[i]
		// An empty ETag means the client omitted it. S3 requires one, but
		// rejecting that outright would break callers this proxy has always
		// accepted, so an omitted ETag skips the comparison and every
		// supplied one is checked.
		if want.ETag != "" && etag.Normalize(want.ETag) != etag.Normalize(clientPartETag(got)) {
			return &core.S3Error{
				StatusCode: http.StatusBadRequest,
				Code:       "InvalidPart",
				Message: fmt.Sprintf("Part number %d has ETag %s, which does not match the uploaded part",
					want.PartNumber, want.ETag),
			}
		}
		// The final part carries whatever remains, so only the parts before
		// it have to meet the floor.
		isFinal := i == len(manifest)-1
		if size := got.ClientSize(got.SizeBytes); enforceMinSize && !isFinal && size < MinPartSizeBytes {
			return &core.S3Error{
				StatusCode: http.StatusBadRequest,
				Code:       "EntityTooSmall",
				Message: fmt.Sprintf("Part number %d is %d bytes, below the %d byte minimum for a non-final part",
					want.PartNumber, size, MinPartSizeBytes),
			}
		}
	}
	return nil
}
