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

// xmlListResultV1 is the XML response for ListObjectsV1: the shared page plus
// its marker paging.
type xmlListResultV1 struct {
	XMLName xml.Name `xml:"ListBucketResult"`
	listPage
	Marker     string `xml:"Marker"`
	NextMarker string `xml:"NextMarker,omitempty"`
}

// xmlListResultV2 is the XML response for ListObjectsV2: the shared page plus
// its key count and token paging.
type xmlListResultV2 struct {
	XMLName xml.Name `xml:"ListBucketResult"`
	listPage
	KeyCount              int    `xml:"KeyCount"`
	ContinuationToken     string `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string `xml:"NextContinuationToken,omitempty"`
	StartAfter            string `xml:"StartAfter,omitempty"`
}

// listPage is what both listing versions answer with, in the client's key
// space: the bucket prefix is stripped from every key and common prefix. Each
// version embeds it, so the shared fields are filled in one place. nextToken
// is where the next page starts, which each version reports its own way.
type listPage struct {
	Xmlns          s3Namespace       `xml:"xmlns,attr"`
	Name           string            `xml:"Name"`
	Prefix         string            `xml:"Prefix"`
	Delimiter      string            `xml:"Delimiter,omitempty"`
	MaxKeys        int               `xml:"MaxKeys"`
	IsTruncated    bool              `xml:"IsTruncated"`
	Contents       []xmlContent      `xml:"Contents"`
	CommonPrefixes []xmlCommonPrefix `xml:"CommonPrefixes,omitempty"`
	nextToken      string
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
// returns the page after the client's startAfter key.
func (s *Server) listBucket(ctx context.Context, t *target, startAfter string) (listPage, error) {
	maxKeys, err := queryCount(t, "max-keys", listingPageLimit, listingPageLimit)
	if err != nil {
		return listPage{}, err
	}

	bucketPrefix := internalkey.Prefix(t.bucket)
	page := listPage{
		Name:      t.bucket,
		Prefix:    t.query.Get("prefix"),
		Delimiter: t.query.Get("delimiter"),
		MaxKeys:   maxKeys,
	}
	if startAfter != "" {
		startAfter = bucketPrefix + startAfter
	}

	result, err := s.Objects.ListObjects(ctx, bucketPrefix+page.Prefix, page.Delimiter, startAfter, maxKeys)
	if err != nil {
		return listPage{}, err
	}

	prefixLen := len(bucketPrefix)
	page.IsTruncated = result.IsTruncated
	if result.NextContinuationToken != "" {
		page.nextToken = result.NextContinuationToken[prefixLen:]
	}
	page.Contents, page.CommonPrefixes = buildListContents(result.Objects, result.CommonPrefixes, prefixLen)
	return page, nil
}

// handleListObjectsV1 processes GET requests at the bucket level without
// list-type=2, returning an S3-compatible ListBucketResult XML response using
// marker-based pagination. Marker is echoed as sent; NextMarker is returned
// only with a delimiter, as S3 does, since a client otherwise resumes from the
// last Key.
func (s *Server) handleListObjectsV1(ctx context.Context, w http.ResponseWriter, t *target) (int, error) {
	marker := t.query.Get("marker")
	page, err := s.listBucket(ctx, t, marker)
	if err != nil {
		return 0, err
	}
	result := xmlListResultV1{listPage: page, Marker: marker}
	if page.Delimiter != "" {
		result.NextMarker = page.nextToken
	}
	return respondXML(w, result)
}

// handleListObjectsV2 processes GET requests at the bucket level, returning an
// S3-compatible ListObjectsV2 XML response. A continuation token takes
// precedence over start-after, and both are echoed as sent.
func (s *Server) handleListObjectsV2(ctx context.Context, w http.ResponseWriter, t *target) (int, error) {
	continuationToken := t.query.Get("continuation-token")
	startAfter := t.query.Get("start-after")
	page, err := s.listBucket(ctx, t, cmp.Or(continuationToken, startAfter))
	if err != nil {
		return 0, err
	}
	return respondXML(w, xmlListResultV2{
		listPage:              page,
		KeyCount:              len(page.Contents) + len(page.CommonPrefixes),
		ContinuationToken:     continuationToken,
		NextContinuationToken: page.nextToken,
		StartAfter:            startAfter,
	})
}
