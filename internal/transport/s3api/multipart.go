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
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	contentType := r.Header.Get("Content-Type")
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
		count, err := s.Manager.Store().CountActiveMultipartUploads(ctx, bucket+"/")
		if err != nil {
			return writeStorageError(w, err, "Failed to check multipart upload count"), err
		}
		if count >= int64(limit) {
			writeS3Error(w, http.StatusServiceUnavailable, "SlowDown", "Too many active multipart uploads")
			return http.StatusServiceUnavailable, errors.New("multipart upload limit reached")
		}
	}

	uploadID, _, err := s.Manager.MultipartManager.CreateMultipartUpload(ctx, internalKey, contentType, metadata)
	if err != nil {
		return writeStorageError(w, err, "Failed to create multipart upload"), err
	}

	result := initiateMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucket,
		Key:      key,
		UploadId: uploadID,
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode create multipart response: %w", err)
	}
	return http.StatusOK, nil
}

// handleUploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X
func (s *Server) handleUploadPart(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	uploadID := r.URL.Query().Get("uploadId")
	partNumberStr := r.URL.Query().Get("partNumber")

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Invalid part number")
		return http.StatusBadRequest, fmt.Errorf("invalid part number: %s", partNumberStr)
	}

	if r.ContentLength < 0 {
		writeS3Error(w, http.StatusLengthRequired, "MissingContentLength", "Content-Length required")
		return http.StatusLengthRequired, fmt.Errorf("missing Content-Length for part upload")
	}

	if s.MaxObjectSize > 0 && r.ContentLength > s.MaxObjectSize {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Part size exceeds the maximum allowed size")
		return http.StatusRequestEntityTooLarge, fmt.Errorf("part size %d exceeds max %d", r.ContentLength, s.MaxObjectSize)
	}

	if s.MaxObjectSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.MaxObjectSize)
	}

	etag, err := s.Manager.MultipartManager.UploadPart(ctx, uploadID, partNumber, r.Body, r.ContentLength)
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

	// Limit the XML body to 1 MB to prevent memory exhaustion from oversized requests.
	var req completeMultipartUploadRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "Failed to parse request body")
		return http.StatusBadRequest, fmt.Errorf("failed to decode complete request: %w", err)
	}

	var partNumbers []int
	for _, p := range req.Parts {
		partNumbers = append(partNumbers, p.PartNumber)
	}

	// Validate total assembled size against MaxObjectSize before the
	// expensive reassembly (read all parts + re-upload combined object).
	if s.MaxObjectSize > 0 {
		parts, err := s.Manager.MultipartManager.GetParts(ctx, uploadID)
		if err != nil {
			return writeStorageError(w, err, "Failed to get parts"), err
		}
		requested := make(map[int]bool, len(partNumbers))
		for _, pn := range partNumbers {
			requested[pn] = true
		}
		var totalSize int64
		for _, p := range parts {
			if requested[p.PartNumber] {
				totalSize += p.SizeBytes
			}
		}
		if totalSize > s.MaxObjectSize {
			writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Combined object size exceeds the maximum allowed size")
			return http.StatusRequestEntityTooLarge, fmt.Errorf("combined size %d exceeds max %d", totalSize, s.MaxObjectSize)
		}
	}

	etag, err := s.Manager.MultipartManager.CompleteMultipartUpload(ctx, uploadID, partNumbers)
	if err != nil {
		return writeStorageError(w, err, "Failed to complete multipart upload"), err
	}

	result := completeMultipartUploadResult{
		Xmlns:  "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket: bucket,
		Key:    key,
		ETag:   etag,
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode complete multipart response: %w", err)
	}
	return http.StatusOK, nil
}

// handleAbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId=X
func (s *Server) handleAbortMultipartUpload(ctx context.Context, w http.ResponseWriter, uploadID string) (int, error) {
	err := s.Manager.MultipartManager.AbortMultipartUpload(ctx, uploadID)
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
	bucketPrefix := bucket + "/"

	maxUploads := parseQueryInt(r, "max-uploads", 1000, 1000)

	// Fetch one extra to detect truncation
	uploads, err := s.Manager.MultipartManager.ListMultipartUploads(ctx, bucketPrefix, maxUploads+1)
	if err != nil {
		return writeStorageError(w, err, "Failed to list multipart uploads"), err
	}

	truncated := len(uploads) > maxUploads
	if truncated {
		uploads = uploads[:maxUploads]
	}

	result := xmlListMultipartUploadsResult{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:      bucket,
		MaxUploads:  maxUploads,
		IsTruncated: truncated,
	}

	for _, u := range uploads {
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

	parts, err := s.Manager.MultipartManager.GetParts(ctx, uploadID)
	if err != nil {
		return writeStorageError(w, err, "Failed to list parts"), err
	}

	result := listPartsResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucket,
		Key:      key,
		UploadId: uploadID,
	}

	for _, p := range parts {
		result.Parts = append(result.Parts, partInfo{
			PartNumber:   p.PartNumber,
			ETag:         p.ETag,
			Size:         p.SizeBytes,
			LastModified: p.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode list parts response: %w", err)
	}
	return http.StatusOK, nil
}
