// -------------------------------------------------------------------------------
// Multipart Manager - Part and Upload-Row Helpers
//
// Author: Alex Freidah
//
// Helpers shared by the upload, complete, and abort paths: the temp key
// template every part is stored under, the bucket-scope guard that prevents
// cross-bucket upload-ID probing, the scoped fetch+validate combo every
// public entry point uses, and the collect+validate logic the complete
// path runs before assembly.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// multipartPartKey returns the temporary object key for a multipart part.
func multipartPartKey(uploadID string, partNumber int) string {
	return "__multipart/" + uploadID + "/" + strconv.Itoa(partNumber)
}

// fetchScopedUpload looks up the multipart upload and verifies it belongs
// to the (bucket, key) the request URL implies. Returns the same 404
// NoSuchUpload error for both missing and out-of-scope rows so callers
// cannot distinguish the two and probe for upload IDs across buckets.
// span must be the operation's pre-existing span (created at the entry
// point). Errors are recorded against it so the operation span shows
// the failure rather than a detached child span.
func (mp *MultipartManager) fetchScopedUpload(ctx context.Context, span trace.Span, bucket, key, uploadID, operation string) (*core.MultipartUpload, error) {
	mu, err := mp.stores.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return nil, mp.classifyWriteError(span, operation, err)
	}
	if err := validateMultipartScope(mu, bucket, key); err != nil {
		observe.RecordSpanError(span, err)
		return nil, err
	}
	return mu, nil
}

// validateMultipartScope returns ErrMultipartUploadNotFound when the
// multipart upload's stored ObjectKey does not match the (bucket, key) the
// caller's request URL implies. The error code is the same one returned for
// a genuinely missing upload so a caller cannot probe for upload IDs across
// bucket boundaries by observing differing failure modes.
func validateMultipartScope(mu *core.MultipartUpload, bucket, key string) error {
	if mu == nil {
		return core.ErrMultipartUploadNotFound
	}
	if mu.ObjectKey != internalkey.Make(bucket, key) {
		return core.ErrMultipartUploadNotFound
	}
	return nil
}

// collectRequestedParts loads every part for uploadID, validates that all
// requested part numbers were uploaded, then returns the requested
// subset sorted in part-number order ready for assembly.
func (mp *MultipartManager) collectRequestedParts(ctx context.Context, span trace.Span, uploadID string, partNumbers []int) ([]core.MultipartPart, error) {
	allParts, err := mp.stores.GetParts(ctx, uploadID)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, err
	}
	uploaded := make(map[int]bool, len(allParts))
	for _, p := range allParts {
		uploaded[p.PartNumber] = true
	}
	var missing []int
	for _, pn := range partNumbers {
		if !uploaded[pn] {
			missing = append(missing, pn)
		}
	}
	if len(missing) > 0 {
		msg := "parts not uploaded: " + formatPartNumbers(missing)
		observe.MarkSpanError(span, msg)
		return nil, &core.S3Error{StatusCode: 400, Code: "InvalidPart", Message: msg}
	}

	requested := make(map[int]bool, len(partNumbers))
	for _, pn := range partNumbers {
		requested[pn] = true
	}
	var parts []core.MultipartPart
	for _, p := range allParts {
		if requested[p.PartNumber] {
			parts = append(parts, p)
		}
	}
	slices.SortFunc(parts, func(a, b core.MultipartPart) int {
		return a.PartNumber - b.PartNumber
	})
	return parts, nil
}

// formatPartNumbers formats a slice of part numbers for error messages.
func formatPartNumbers(parts []int) string {
	s := make([]string, len(parts))
	for i, pn := range parts {
		s[i] = strconv.Itoa(pn)
	}
	return strings.Join(s, ", ")
}
