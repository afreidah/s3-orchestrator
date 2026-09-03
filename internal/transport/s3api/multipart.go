// -------------------------------------------------------------------------------
// Multipart Upload Handlers - S3 Multipart Upload Protocol
//
// Author: Alex Freidah
//
// HTTP handlers for S3 multipart upload operations. Supports creating uploads,
// uploading parts, completing uploads (reassembly), aborting uploads, listing
// parts, and listing active multipart uploads. Parts are stored under temporary
// keys and concatenated on completion.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// XML TYPES
// -------------------------------------------------------------------------

// initiateMultipartUploadResult is the XML response for CreateMultipartUpload.
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadId string   `xml:"UploadId"`
}

// completeMultipartUploadRequest is the XML request body for CompleteMultipartUpload.
type completeMultipartUploadRequest struct {
	Parts []completePart `xml:"Part"`
}

// completePart identifies a part in a CompleteMultipartUpload request.
type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// completeMultipartUploadResult is the XML response for CompleteMultipartUpload.
type completeMultipartUploadResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}

// copyPartResult is the XML response for UploadPartCopy. UploadPart answers
// with a bare ETag header; the copy form is specified to return the ETag in a
// document, which is what SDKs read the part's validator out of.
type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// listPartsResult is the XML response for ListParts.
type listPartsResult struct {
	XMLName  xml.Name   `xml:"ListPartsResult"`
	Xmlns    string     `xml:"xmlns,attr"`
	Bucket   string     `xml:"Bucket"`
	Key      string     `xml:"Key"`
	UploadId string     `xml:"UploadId"`
	Parts    []partInfo `xml:"Part"`
}

// partInfo holds part metadata for the ListParts response.
type partInfo struct {
	PartNumber   int    `xml:"PartNumber"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

// -------------------------------------------------------------------------
// HANDLERS
// -------------------------------------------------------------------------

// handleCreateMultipartUpload handles POST /{bucket}/{key}?uploads
// key is the user-facing key (for XML response), internalKey is the prefixed
// key used for storage.
func (s *Server) handleCreateMultipartUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket, key, internalKey string) (int, error) {
	contentType := r.Header.Get(headerContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	metadata := extractUserMetadata(r.Header)
	if len(metadata) > 0 {
		if err := validateUserMetadata(metadata); err != nil {
			writeS3Error(w, http.StatusBadRequest, "MetadataTooLarge", err.Error())
			return http.StatusBadRequest, err
		}
	}

	// Check per-bucket multipart upload limit
	if limit := s.GetBucketAuth().MaxMultipartUploads(bucket); limit > 0 {
		count, err := s.Multipart.CountActiveMultipartUploads(ctx, internalkey.Prefix(bucket))
		if err != nil {
			return writeStorageError(w, err, "Failed to check multipart upload count"), err
		}
		if count >= int64(limit) {
			writeS3Error(w, http.StatusServiceUnavailable, "SlowDown", "Too many active multipart uploads")
			return http.StatusServiceUnavailable, errors.New("multipart upload limit reached")
		}
	}

	// Refused before the upload is opened, so an unusable tag set costs no
	// upload slot and no parts: the alternative is discovering it at complete,
	// after the client has transferred everything.
	tags, err := parseTaggingHeader(r.Header.Get("x-amz-tagging"))
	if err != nil {
		return writeTaggingError(w, err), err
	}

	uploadID, _, err := s.Multipart.CreateMultipartUpload(ctx, &multipart.CreateUploadRequest{
		Key:         internalKey,
		ContentType: contentType,
		Metadata:    metadata,
		Tags:        tags,
	})
	if err != nil {
		return writeStorageError(w, err, "Failed to create multipart upload"), err
	}

	result := initiateMultipartUploadResult{
		Xmlns:    s3XMLNS,
		Bucket:   bucket,
		Key:      key,
		UploadId: uploadID,
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode create multipart response: %w", err)
	}
	return http.StatusOK, nil
}

// errCopyRangeMalformed and errCopyRangeUnsatisfiable separate a copy-source
// range this server cannot read from one it reads fine but the source object
// cannot satisfy. S3 answers the two with different status codes.
var (
	errCopyRangeMalformed     = errors.New("malformed x-amz-copy-source-range")
	errCopyRangeUnsatisfiable = errors.New("x-amz-copy-source-range lies outside the source object")
)

// parsePartNumber reads the partNumber query parameter both part-upload forms
// require. ok=false means the response has already been written and the caller
// must propagate the (status, error) unchanged.
func parsePartNumber(w http.ResponseWriter, r *http.Request) (int, int, error, bool) {
	partNumberStr := r.URL.Query().Get("partNumber")
	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < multipart.MinPartNumber || partNumber > multipart.MaxPartNumber {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Invalid part number")
		return 0, http.StatusBadRequest, fmt.Errorf("invalid part number: %s", partNumberStr), false
	}
	return partNumber, 0, nil, true
}

// parseCopySourceRange resolves an x-amz-copy-source-range against a source of
// sourceSize bytes, returning the Range header the source is read with and the
// number of bytes that selects. An absent range copies the whole object.
//
// Only the closed "bytes=first-last" form is accepted, which is the only form
// UploadPartCopy is specified to take: the part's length has to be known before
// the read begins, so an open-ended or suffix range has nothing to mean here.
func parseCopySourceRange(spec string, sourceSize int64) (string, int64, error) {
	if spec == "" {
		return "", sourceSize, nil
	}
	bounds, found := strings.CutPrefix(spec, "bytes=")
	if !found {
		return "", 0, errCopyRangeMalformed
	}
	firstStr, lastStr, found := strings.Cut(bounds, "-")
	if !found {
		return "", 0, errCopyRangeMalformed
	}
	first, firstErr := strconv.ParseInt(firstStr, 10, 64)
	last, lastErr := strconv.ParseInt(lastStr, 10, 64)
	if firstErr != nil || lastErr != nil || first < 0 || last < first {
		return "", 0, errCopyRangeMalformed
	}
	if last >= sourceSize {
		return "", 0, errCopyRangeUnsatisfiable
	}
	return fmt.Sprintf("bytes=%d-%d", first, last), last - first + 1, nil
}

// writeCopySourceRangeError renders a copy-source range failure: one this
// server cannot parse is the caller's mistake, one the source cannot satisfy
// is a 416 against that object.
func writeCopySourceRangeError(w http.ResponseWriter, err error) (int, error) {
	if errors.Is(err, errCopyRangeUnsatisfiable) {
		writeS3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange",
			"The x-amz-copy-source-range is not satisfiable for the source object")
		return http.StatusRequestedRangeNotSatisfiable, err
	}
	writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-copy-source-range")
	return http.StatusBadRequest, err
}

// handleUploadPartCopy handles PUT /{bucket}/{key}?partNumber=N&uploadId=X
// carrying X-Amz-Copy-Source: the part's bytes come from a range of an object
// that already exists rather than from the request body. This is how a client
// copies server-side above the multipart threshold.
//
// The bytes stream through the orchestrator rather than taking a backend-native
// copy, because the part is stored under the upload's own part key, which no
// backend-side CopySource can name.
func (s *Server) handleUploadPartCopy(ctx context.Context, w http.ResponseWriter, r *http.Request, rk *objectRouteKey, copySource string) (int, error) {
	partNumber, status, err, ok := parsePartNumber(w, r)
	if !ok {
		return status, err
	}

	sourceKey, status, err, ok := resolveCopySource(w, rk.bucket, copySource)
	if !ok {
		return status, err
	}

	// HEAD first: the range is validated against the source's real length, so
	// an out-of-bounds copy costs nothing and the caller learns which end of
	// the request was wrong.
	head, err := s.Objects.HeadObject(ctx, sourceKey)
	if err != nil {
		return writeStorageError(w, err, "Failed to read copy source"), err
	}

	rangeHeader, size, err := parseCopySourceRange(r.Header.Get(headerCopySourceRange), head.Size)
	if err != nil {
		return writeCopySourceRangeError(w, err)
	}
	if s.MaxObjectSize > 0 && size > s.MaxObjectSize {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Part size exceeds the maximum allowed size")
		return http.StatusRequestEntityTooLarge, fmt.Errorf("copied part size %d exceeds max %d", size, s.MaxObjectSize)
	}

	source, err := s.Objects.GetObject(ctx, sourceKey, rangeHeader)
	if err != nil {
		return writeStorageError(w, err, "Failed to read copy source"), err
	}
	defer source.Body.Close()

	// The stream reports the length the part is stored with, rather than the
	// range arithmetic deciding it: an encrypted or compressed source is served
	// as plaintext, and only the read path knows what a range over it resolves
	// to. It falls back to the computed size when the read path says nothing.
	partSize := source.Size
	if partSize <= 0 {
		partSize = size
	}

	etag, err := s.Multipart.UploadPart(ctx, rk.bucket, rk.key, rk.uploadID, partNumber, source.Body, partSize)
	if err != nil {
		return writeStorageError(w, err, "Failed to copy part"), err
	}

	result := copyPartResult{
		Xmlns:        s3XMLNS,
		ETag:         etag,
		LastModified: head.LastModified.UTC().Format(time.RFC3339),
	}
	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode copy part response: %w", err)
	}
	return http.StatusOK, nil
}

// handleUploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X.
// bucket and key scope the upload to the request URL so an attacker holding
// credentials for one bucket cannot write parts to a multipart upload that
// belongs to another (the manager rejects with 404 NoSuchUpload).
func (s *Server) handleUploadPart(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket, key string) (int, error) {
	uploadID := r.URL.Query().Get("uploadId")

	partNumber, status, err, ok := parsePartNumber(w, r)
	if !ok {
		return status, err
	}

	if status, err, ok := enforceContentLength(w, r, s.MaxObjectSize, "Part"); !ok {
		return status, err
	}

	etag, err := s.Multipart.UploadPart(ctx, bucket, key, uploadID, partNumber, r.Body, r.ContentLength)
	if err != nil {
		return writeStorageError(w, err, "Failed to upload part"), err
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleCompleteMultipartUpload handles POST /{bucket}/{key}?uploadId=X
func (s *Server) handleCompleteMultipartUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket, key string) (int, error) {
	uploadID := r.URL.Query().Get("uploadId")

	var req completeMultipartUploadRequest
	if status, err := decodeXMLBody(w, r, maxCompleteMultipartBody, &req); err != nil {
		return status, fmt.Errorf("complete multipart upload: %w", err)
	}

	// Carry the client's ETags through: the manager compares each one to the
	// stored part so a stale manifest is rejected rather than assembled.
	manifest := make([]core.CompletePart, len(req.Parts))
	partNumbers := make([]int, len(req.Parts))
	for i, p := range req.Parts {
		manifest[i] = core.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
		partNumbers[i] = p.PartNumber
	}

	if status, err := s.checkMultipartTotalSize(ctx, w, bucket, key, uploadID, partNumbers); err != nil {
		return status, err
	}

	// CompleteMultipartUpload is the moment a multipart upload becomes a
	// resolvable key, so the precondition is evaluated here, not at
	// CreateMultipartUpload (parts can be uploaded against a key that is
	// only later observed to already exist).
	if status, err, done := s.checkIfNoneMatchStar(ctx, w, r, key); done {
		return status, err
	}

	etag, err := s.Multipart.CompleteMultipartUpload(ctx, bucket, key, uploadID, manifest)
	if err != nil {
		return writeStorageError(w, err, "Failed to complete multipart upload"), err
	}

	result := completeMultipartUploadResult{
		Xmlns:  s3XMLNS,
		Bucket: bucket,
		Key:    key,
		ETag:   etag,
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode complete multipart response: %w", err)
	}
	return http.StatusOK, nil
}

// checkMultipartTotalSize validates the combined assembled size against
// MaxObjectSize before the expensive reassembly path runs (read all
// parts + re-upload combined object). Returns (0, nil) when the limit is
// disabled or the assembly is within bounds. bucket/key scope the
// underlying GetParts call to the request URL.
func (s *Server) checkMultipartTotalSize(ctx context.Context, w http.ResponseWriter, bucket, key, uploadID string, partNumbers []int) (int, error) {
	if s.MaxObjectSize <= 0 {
		return 0, nil
	}
	parts, err := s.Multipart.GetParts(ctx, bucket, key, uploadID)
	if err != nil {
		return writeStorageError(w, err, "Failed to get parts"), err
	}
	requested := make(map[int]bool, len(partNumbers))
	for _, pn := range partNumbers {
		requested[pn] = true
	}
	var totalSize int64
	for i := range parts {
		if requested[parts[i].PartNumber] {
			totalSize += parts[i].SizeBytes
		}
	}
	if totalSize > s.MaxObjectSize {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Combined object size exceeds the maximum allowed size")
		return http.StatusRequestEntityTooLarge, fmt.Errorf("combined size %d exceeds max %d", totalSize, s.MaxObjectSize)
	}
	return 0, nil
}

// handleAbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId=X.
// bucket and key scope the abort to the request URL so a caller for one
// bucket cannot wipe an in-flight upload that belongs to another.
func (s *Server) handleAbortMultipartUpload(ctx context.Context, w http.ResponseWriter, bucket, key, uploadID string) (int, error) {
	err := s.Multipart.AbortMultipartUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return writeStorageError(w, err, "Failed to abort multipart upload"), err
	}

	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent, nil
}

// xmlListMultipartUploadsResult is the XML response for ListMultipartUploads.
type xmlListMultipartUploadsResult struct {
	XMLName     xml.Name    `xml:"ListMultipartUploadsResult"`
	Xmlns       string      `xml:"xmlns,attr"`
	Bucket      string      `xml:"Bucket"`
	MaxUploads  int         `xml:"MaxUploads"`
	IsTruncated bool        `xml:"IsTruncated"`
	Upload      []xmlUpload `xml:"Upload"`
}

// xmlUpload holds a single multipart upload entry for the list response.
type xmlUpload struct {
	Key       string `xml:"Key"`
	UploadId  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"`
}

// handleListMultipartUploads handles GET /{bucket}?uploads, returning active
// multipart uploads scoped to the bucket. Strips the internal bucket prefix
// from keys before returning to clients.
func (s *Server) handleListMultipartUploads(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket string) (int, error) {
	bucketPrefix := internalkey.Prefix(bucket)

	maxUploads := parseQueryInt(r, "max-uploads", 1000, 1000)

	// Fetch one extra to detect truncation
	uploads, err := s.Multipart.ListMultipartUploads(ctx, bucketPrefix, maxUploads+1)
	if err != nil {
		return writeStorageError(w, err, "Failed to list multipart uploads"), err
	}

	truncated := len(uploads) > maxUploads
	if truncated {
		uploads = uploads[:maxUploads]
	}

	result := xmlListMultipartUploadsResult{
		Xmlns:       s3XMLNS,
		Bucket:      bucket,
		MaxUploads:  maxUploads,
		IsTruncated: truncated,
	}

	for i := range uploads {
		u := &uploads[i]
		result.Upload = append(result.Upload, xmlUpload{
			Key:       strings.TrimPrefix(u.ObjectKey, bucketPrefix),
			UploadId:  u.UploadID,
			Initiated: u.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode list multipart uploads response: %w", err)
	}
	return http.StatusOK, nil
}

// handleListParts handles GET /{bucket}/{key}?uploadId=X
// key is the user-facing key (for XML response), internalKey is the prefixed
// key (unused here since GetParts uses uploadID, but accepted for consistency).
func (s *Server) handleListParts(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket, key, _ string) (int, error) {
	uploadID := r.URL.Query().Get("uploadId")

	parts, err := s.Multipart.GetParts(ctx, bucket, key, uploadID)
	if err != nil {
		return writeStorageError(w, err, "Failed to list parts"), err
	}

	result := listPartsResult{
		Xmlns:    s3XMLNS,
		Bucket:   bucket,
		Key:      key,
		UploadId: uploadID,
	}

	for i := range parts {
		result.Parts = append(result.Parts, partInfo{
			PartNumber:   parts[i].PartNumber,
			ETag:         parts[i].ETag,
			Size:         parts[i].SizeBytes,
			LastModified: parts[i].CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode list parts response: %w", err)
	}
	return http.StatusOK, nil
}
