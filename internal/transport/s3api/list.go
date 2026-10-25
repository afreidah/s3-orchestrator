// -------------------------------------------------------------------------------
// List Handlers - S3 ListObjectsV1 and ListObjectsV2
//
// Author: Alex Freidah
//
// HTTP handlers for the S3 ListObjects operations. V2 uses continuation tokens
// for pagination; V1 uses marker-based pagination. Both return XML responses
// compatible with S3 clients, supporting prefix filtering, delimiter-based
// directory grouping. Translates between external user-facing keys and
// internal prefixed keys.
// -------------------------------------------------------------------------------

package s3api

import (
	"cmp"
	"context"
	"encoding/xml"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// xmlContent represents a single object in an S3 ListBucketResult response.
// ETag and StorageClass are required by the S3 REST API spec; aws-sdk-go-v2
// models ETag as *string and dereferences it without nil-checks, so omitting
// the element crashes clients like aptly mid-list.
type xmlContent struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	StorageClass string `xml:"StorageClass"`
}

// listStorageClass is the storage class reported for every object in a
// ListObjects response. S3 requires a value per Contents entry and
// STANDARD is the universal default.
const listStorageClass = "STANDARD"

// xmlCommonPrefix represents a common prefix (directory) entry in a
// ListBucketResult response.
type xmlCommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// xmlListResultV1 is the XML response for ListObjectsV1.
type xmlListResultV1 struct {
	XMLName        xml.Name          `xml:"ListBucketResult"`
	Xmlns          string            `xml:"xmlns,attr"`
	Name           string            `xml:"Name"`
	Prefix         string            `xml:"Prefix"`
	Marker         string            `xml:"Marker"`
	NextMarker     string            `xml:"NextMarker,omitempty"`
	Delimiter      string            `xml:"Delimiter,omitempty"`
	MaxKeys        int               `xml:"MaxKeys"`
	IsTruncated    bool              `xml:"IsTruncated"`
	Contents       []xmlContent      `xml:"Contents"`
	CommonPrefixes []xmlCommonPrefix `xml:"CommonPrefixes,omitempty"`
}

// xmlListResultV2 is the XML response for ListObjectsV2.
type xmlListResultV2 struct {
	XMLName               xml.Name          `xml:"ListBucketResult"`
	Xmlns                 string            `xml:"xmlns,attr"`
	Name                  string            `xml:"Name"`
	Prefix                string            `xml:"Prefix"`
	Delimiter             string            `xml:"Delimiter,omitempty"`
	MaxKeys               int               `xml:"MaxKeys"`
	KeyCount              int               `xml:"KeyCount"`
	IsTruncated           bool              `xml:"IsTruncated"`
	ContinuationToken     string            `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string            `xml:"NextContinuationToken,omitempty"`
	StartAfter            string            `xml:"StartAfter,omitempty"`
	Contents              []xmlContent      `xml:"Contents"`
	CommonPrefixes        []xmlCommonPrefix `xml:"CommonPrefixes,omitempty"`
}

// listPage is one ListObjects page in the client's key space: the bucket
// prefix is stripped from every key, common prefix, and continuation token,
// so each handler only arranges it into its own response shape.
type listPage struct {
	prefix         string
	delimiter      string
	maxKeys        int
	keyCount       int
	isTruncated    bool
	nextToken      string
	contents       []xmlContent
	commonPrefixes []xmlCommonPrefix
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// buildListContents converts storage objects and common prefixes to their XML
// representations, stripping the internal bucket prefix from each key.
// prefixLen is len(bucket + "/")  -  used for zero-copy string slicing.
func buildListContents(objects []core.ObjectLocation, prefixes []string, prefixLen int) ([]xmlContent, []xmlCommonPrefix) {
	contents := make([]xmlContent, 0, len(objects))
	for i := range objects {
		// The object's own ETag, not the integrity hash: that one is a SHA-256
		// of the stored bytes and never was an ETag. An object that has not
		// learned one yet reports the empty pair of quotes, which is what
		// clients that dereference the element without a nil check need.
		etag := `""`
		if id := objects[i].Identity; id.Complete() {
			etag = id.ETag
		}
		contents = append(contents, xmlContent{
			Key:          objects[i].ObjectKey[prefixLen:],
			Size:         objects[i].SizeBytes,
			LastModified: objects[i].CreatedAt.UTC().Format(time.RFC3339),
			ETag:         etag,
			StorageClass: listStorageClass,
		})
	}
	commonPrefixes := make([]xmlCommonPrefix, 0, len(prefixes))
	for _, cp := range prefixes {
		commonPrefixes = append(commonPrefixes, xmlCommonPrefix{
			Prefix: cp[prefixLen:],
		})
	}
	return contents, commonPrefixes
}

// listBucket reads the prefix, delimiter, and max-keys a listing shares and
// returns the page after the client's startAfter key. A failure has already
// been written to w, and the returned status is what the handler reports.
func (s *Server) listBucket(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket, startAfter string) (*listPage, int, error) {
	maxKeys, err := parseListingCount(r, "max-keys", listingPageLimit, listingPageLimit)
	if err != nil {
		status, err := invalidArgument(w, err)
		return nil, status, err
	}

	bucketPrefix := internalkey.Prefix(bucket)
	page := &listPage{
		prefix:    r.URL.Query().Get("prefix"),
		delimiter: r.URL.Query().Get("delimiter"),
		maxKeys:   maxKeys,
	}
	if startAfter != "" {
		startAfter = bucketPrefix + startAfter
	}

	result, err := s.Objects.ListObjects(ctx, bucketPrefix+page.prefix, page.delimiter, startAfter, maxKeys)
	if err != nil {
		return nil, writeStorageError(w, err, "Failed to list objects"), err
	}

	prefixLen := len(bucketPrefix)
	page.keyCount = result.KeyCount()
	page.isTruncated = result.IsTruncated
	if result.NextContinuationToken != "" {
		page.nextToken = result.NextContinuationToken[prefixLen:]
	}
	page.contents, page.commonPrefixes = buildListContents(result.Objects, result.CommonPrefixes, prefixLen)
	return page, http.StatusOK, nil
}

// handleListObjectsV1 processes GET requests at the bucket level without
// list-type=2, returning an S3-compatible ListBucketResult XML response using
// marker-based pagination. Marker is echoed as sent; NextMarker is returned
// only with a delimiter, as S3 does, since a client otherwise resumes from the
// last Key.
func (s *Server) handleListObjectsV1(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket string) (int, error) {
	marker := r.URL.Query().Get("marker")
	page, status, err := s.listBucket(ctx, w, r, bucket, marker)
	if err != nil {
		return status, err
	}

	xmlResult := xmlListResultV1{
		Xmlns:          s3XMLNS,
		Name:           bucket,
		Prefix:         page.prefix,
		Marker:         marker,
		Delimiter:      page.delimiter,
		MaxKeys:        page.maxKeys,
		IsTruncated:    page.isTruncated,
		Contents:       page.contents,
		CommonPrefixes: page.commonPrefixes,
	}
	if page.delimiter != "" {
		xmlResult.NextMarker = page.nextToken
	}
	return respondXML(w, xmlResult)
}

// handleListObjectsV2 processes GET requests at the bucket level, returning an
// S3-compatible ListObjectsV2 XML response. A continuation token takes
// precedence over start-after, and both are echoed as sent.
func (s *Server) handleListObjectsV2(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket string) (int, error) {
	continuationToken := r.URL.Query().Get("continuation-token")
	startAfter := r.URL.Query().Get("start-after")
	page, status, err := s.listBucket(ctx, w, r, bucket, cmp.Or(continuationToken, startAfter))
	if err != nil {
		return status, err
	}

	return respondXML(w, xmlListResultV2{
		Xmlns:                 s3XMLNS,
		Name:                  bucket,
		Prefix:                page.prefix,
		Delimiter:             page.delimiter,
		MaxKeys:               page.maxKeys,
		KeyCount:              page.keyCount,
		IsTruncated:           page.isTruncated,
		ContinuationToken:     continuationToken,
		NextContinuationToken: page.nextToken,
		StartAfter:            startAfter,
		Contents:              page.contents,
		CommonPrefixes:        page.commonPrefixes,
	})
}
