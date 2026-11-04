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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/byterange"
)

// listingPageLimit is the most objects, parts or uploads one S3 listing
// response returns, and the page size when the client names none.
const listingPageLimit = 1000

// -------------------------------------------------------------------------
// XML TYPES
// -------------------------------------------------------------------------

// initiateMultipartUploadResult is the XML response for CreateMultipartUpload.
type initiateMultipartUploadResult struct {
	XMLName  xml.Name    `xml:"InitiateMultipartUploadResult"`
	Xmlns    s3Namespace `xml:"xmlns,attr"`
	Bucket   string      `xml:"Bucket"`
	Key      string      `xml:"Key"`
	UploadId string      `xml:"UploadId"`
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
	XMLName xml.Name    `xml:"CompleteMultipartUploadResult"`
	Xmlns   s3Namespace `xml:"xmlns,attr"`
	Bucket  string      `xml:"Bucket"`
	Key     string      `xml:"Key"`
	ETag    string      `xml:"ETag"`
}

// listPartsResult is the XML response for ListParts.
type listPartsResult struct {
	XMLName              xml.Name    `xml:"ListPartsResult"`
	Xmlns                s3Namespace `xml:"xmlns,attr"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadId             string      `xml:"UploadId"`
	PartNumberMarker     int         `xml:"PartNumberMarker"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	Parts                []partInfo  `xml:"Part"`
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

// handleCreateMultipartUpload handles POST /{bucket}/{key}?uploads. The
// response names the client's key; the upload is opened under the internal one.
func (s *Server) handleCreateMultipartUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	contentType, metadata, err := uploadHeaders(r)
	if err != nil {
		return 0, err
	}
	if err := s.checkUploadLimit(ctx, t.bucket); err != nil {
		return 0, err
	}

	// Refused before the upload is opened, so an unusable tag set costs no
	// upload slot and no parts: the alternative is discovering it at complete,
	// after the client has transferred everything.
	tags, err := parseTaggingHeader(r.Header.Get("x-amz-tagging"))
	if err != nil {
		return 0, err
	}

	uploadID, _, err := s.Multipart.CreateMultipartUpload(ctx, &multipart.CreateUploadRequest{
		Key:         t.internalKey,
		ContentType: contentType,
		Metadata:    metadata,
		Tags:        tags,
	})
	if err != nil {
		return 0, err
	}

	return respondXML(w, initiateMultipartUploadResult{Bucket: t.bucket, Key: t.key, UploadId: uploadID})
}

// errUploadLimit refuses a new upload to a bucket already holding as many open
// uploads as it allows.
var errUploadLimit = s3Err(http.StatusServiceUnavailable, "SlowDown", "Too many active multipart uploads")

// checkUploadLimit refuses a new upload to a bucket at its configured ceiling of
// open uploads. A bucket with no ceiling is never counted.
func (s *Server) checkUploadLimit(ctx context.Context, bucket string) error {
	limit := s.registry.Load().MaxMultipartUploads(bucket)
	if limit <= 0 {
		return nil
	}
	count, err := s.Multipart.CountActiveMultipartUploads(ctx, internalkey.Prefix(bucket))
	if err != nil {
		return err
	}
	if count >= int64(limit) {
		return errUploadLimit
	}
	return nil
}

// errCopyRangeMalformed and errCopyRangeUnsatisfiable separate a copy-source
// range this server cannot read from one it reads fine but the source object
// cannot satisfy: the caller's mistake, or a 416 against that object.
var (
	errCopyRangeMalformed     = s3Err(http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-copy-source-range")
	errCopyRangeUnsatisfiable = s3Err(http.StatusRequestedRangeNotSatisfiable, "InvalidRange",
		"The x-amz-copy-source-range is not satisfiable for the source object")
)

// parsePartNumber reads the partNumber query parameter both part-upload forms
// require. It is parsed at 32 bits because the stores keep it in an int32.
func parsePartNumber(t *target) (int, error) {
	raw := t.query.Get("partNumber")
	partNumber, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || !core.ValidPartNumber(int(partNumber)) {
		return 0, fmt.Errorf("%w: %q", s3Err(http.StatusBadRequest, "InvalidArgument", "Invalid part number"), raw)
	}
	return int(partNumber), nil
}

// parseCopySourceRange resolves an x-amz-copy-source-range against a source of
// sourceSize bytes, returning the Range header the source is read with and the
// number of bytes that selects. An absent range copies the whole object. Only
// the closed "bytes=first-last" form is accepted, as UploadPartCopy specifies.
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
	return byterange.Header(first, last), last - first + 1, nil
}

// handleUploadPartCopy handles PUT /{bucket}/{key}?partNumber=N&uploadId=X
// carrying X-Amz-Copy-Source: the part's bytes come from a range of an object
// that already exists rather than from the request body. The bytes stream
// through the orchestrator because the part is stored under the upload's own
// part key, which no backend-side CopySource can name.
func (s *Server) handleUploadPartCopy(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	partNumber, err := parsePartNumber(t)
	if err != nil {
		return 0, err
	}
	sourceKey, err := resolveCopySource(r, t)
	if err != nil {
		return 0, err
	}

	// HEAD first: the range is validated against the source's real length, so
	// an out-of-bounds copy costs nothing and the caller learns which end of
	// the request was wrong.
	head, err := s.Objects.HeadObject(ctx, sourceKey)
	if err != nil {
		return 0, err
	}

	rangeHeader, size, err := parseCopySourceRange(r.Header.Get(headerCopySourceRange), head.Size)
	if err != nil {
		return 0, err
	}
	if err := s.checkSize(size, "Part"); err != nil {
		return 0, err
	}

	source, err := s.Objects.GetObject(ctx, sourceKey, rangeHeader)
	if err != nil {
		return 0, err
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

	etag, err := s.Multipart.UploadPart(ctx, t.bucket, t.key, t.uploadID, partNumber, source.Body, partSize)
	if err != nil {
		return 0, err
	}

	return respondCopy(w, "CopyPartResult", etag, head.LastModified)
}

// handleUploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X.
// bucket and key scope the upload to the request URL so an attacker holding
// credentials for one bucket cannot write parts to a multipart upload that
// belongs to another (the manager rejects with 404 NoSuchUpload).
func (s *Server) handleUploadPart(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	partNumber, err := parsePartNumber(t)
	if err != nil {
		return 0, err
	}
	if err := s.acceptBody(w, r, "Part"); err != nil {
		return 0, err
	}

	etag, err := s.Multipart.UploadPart(ctx, t.bucket, t.key, t.uploadID, partNumber, r.Body, r.ContentLength)
	if err != nil {
		return 0, err
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleCompleteMultipartUpload handles POST /{bucket}/{key}?uploadId=X
func (s *Server) handleCompleteMultipartUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	var req completeMultipartUploadRequest
	if err := decodeXMLBody(w, r, maxCompleteMultipartBody, &req); err != nil {
		return 0, err
	}

	// Carry the client's ETags through: the manager compares each one to the
	// stored part so a stale manifest is rejected rather than assembled.
	manifest := make([]core.CompletePart, len(req.Parts))
	partNumbers := make([]int, len(req.Parts))
	for i, p := range req.Parts {
		manifest[i] = core.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
		partNumbers[i] = p.PartNumber
	}

	if err := s.checkAssemblySize(ctx, t, partNumbers); err != nil {
		return 0, err
	}

	// CompleteMultipartUpload is the moment a multipart upload becomes a
	// resolvable key, so the precondition is evaluated here, not at
	// CreateMultipartUpload (parts can be uploaded against a key that is
	// only later observed to already exist).
	if err := s.checkIfNoneMatchStar(ctx, r, t.internalKey); err != nil {
		return 0, err
	}

	etag, err := s.Multipart.CompleteMultipartUpload(ctx, t.bucket, t.key, t.uploadID, manifest)
	if err != nil {
		return 0, err
	}

	return respondXML(w, completeMultipartUploadResult{Bucket: t.bucket, Key: t.key, ETag: etag})
}

// checkAssemblySize refuses a completion whose named parts add up past the
// object ceiling, before the expensive reassembly reads them all back. Nothing
// is fetched when no ceiling is configured.
func (s *Server) checkAssemblySize(ctx context.Context, t *target, partNumbers []int) error {
	if s.MaxObjectSize <= 0 {
		return nil
	}
	parts, err := s.Multipart.GetParts(ctx, t.bucket, t.key, t.uploadID)
	if err != nil {
		return err
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
	return s.checkSize(totalSize, "Combined object")
}

// handleAbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId=X.
// bucket and key scope the abort to the request URL so a caller for one
// bucket cannot wipe an in-flight upload that belongs to another.
func (s *Server) handleAbortMultipartUpload(ctx context.Context, w http.ResponseWriter, t *target) (int, error) {
	if err := s.Multipart.AbortMultipartUpload(ctx, t.bucket, t.key, t.uploadID); err != nil {
		return 0, err
	}

	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent, nil
}

// xmlListMultipartUploadsResult is the XML response for ListMultipartUploads.
type xmlListMultipartUploadsResult struct {
	XMLName     xml.Name    `xml:"ListMultipartUploadsResult"`
	Xmlns       s3Namespace `xml:"xmlns,attr"`
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
func (s *Server) handleListMultipartUploads(ctx context.Context, w http.ResponseWriter, t *target) (int, error) {
	bucketPrefix := internalkey.Prefix(t.bucket)

	maxUploads, err := queryCount(t, "max-uploads", listingPageLimit, listingPageLimit)
	if err != nil {
		return 0, err
	}

	// Fetch one extra to detect truncation
	uploads, err := s.Multipart.ListMultipartUploads(ctx, bucketPrefix, maxUploads+1)
	if err != nil {
		return 0, err
	}
	uploads, truncated := core.TrimPage(uploads, maxUploads)

	result := xmlListMultipartUploadsResult{
		Bucket:      t.bucket,
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

	return respondXML(w, result)
}

// handleListParts handles GET /{bucket}/{key}?uploadId=X, returning one page
// of parts: up to max-parts numbered above part-number-marker. key is the
// user-facing key for the XML response. NextPartNumberMarker is the last part
// returned, or 0 when the page is empty, and is reported even when the page is
// not truncated, as S3 does.
func (s *Server) handleListParts(ctx context.Context, w http.ResponseWriter, t *target) (int, error) {
	maxParts, err := queryCount(t, "max-parts", listingPageLimit, listingPageLimit)
	if err != nil {
		return 0, err
	}
	marker, err := queryCount(t, "part-number-marker", 0, httputil.NoCeiling)
	if err != nil {
		return 0, err
	}

	parts, truncated, err := s.Multipart.ListParts(ctx, t.bucket, t.key, t.uploadID, marker, maxParts)
	if err != nil {
		return 0, err
	}

	result := listPartsResult{
		Bucket:           t.bucket,
		Key:              t.key,
		UploadId:         t.uploadID,
		PartNumberMarker: marker,
		MaxParts:         maxParts,
		IsTruncated:      truncated,
	}

	for i := range parts {
		result.Parts = append(result.Parts, partInfo{
			PartNumber:   parts[i].PartNumber,
			ETag:         parts[i].ETag,
			Size:         parts[i].SizeBytes,
			LastModified: parts[i].CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	if len(parts) > 0 {
		result.NextPartNumberMarker = parts[len(parts)-1].PartNumber
	}

	return respondXML(w, result)
}
