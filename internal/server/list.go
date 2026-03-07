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

package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/storage"
)

// xmlContent represents a single object in an S3 ListBucketResult response.
type xmlContent struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

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
	Contents              []xmlContent      `xml:"Contents"`
	CommonPrefixes        []xmlCommonPrefix `xml:"CommonPrefixes,omitempty"`
}

// buildListContents converts storage objects and common prefixes to their XML
// representations, stripping the internal bucket prefix from each key.
func buildListContents(objects []storage.ObjectLocation, prefixes []string, bucketPrefix string) ([]xmlContent, []xmlCommonPrefix) {
	contents := make([]xmlContent, 0, len(objects))
	for _, obj := range objects {
		contents = append(contents, xmlContent{
			Key:          strings.TrimPrefix(obj.ObjectKey, bucketPrefix),
			Size:         obj.SizeBytes,
			LastModified: obj.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	commonPrefixes := make([]xmlCommonPrefix, 0, len(prefixes))
	for _, cp := range prefixes {
		commonPrefixes = append(commonPrefixes, xmlCommonPrefix{
			Prefix: strings.TrimPrefix(cp, bucketPrefix),
		})
	}
	return contents, commonPrefixes
}

// handleListObjectsV1 processes GET requests at the bucket level without
// list-type=2, returning an S3-compatible ListBucketResult XML response using
// marker-based pagination. Internally prefixes queries with the bucket name and
// strips the prefix from results before returning to clients.
func (s *Server) handleListObjectsV1(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket string) (int, error) {
	bucketPrefix := bucket + "/"

	userPrefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	marker := r.URL.Query().Get("marker")
	maxKeys := parseQueryInt(r, "max-keys", 1000, 1000)

	internalPrefix := bucketPrefix + userPrefix

	startAfter := ""
	if marker != "" {
		startAfter = bucketPrefix + marker
	}

	result, err := s.Manager.ListObjects(ctx, internalPrefix, delimiter, startAfter, maxKeys)
	if err != nil {
		return writeStorageError(w, err, "Failed to list objects"), err
	}

	nextMarker := ""
	if result.NextContinuationToken != "" {
		nextMarker = strings.TrimPrefix(result.NextContinuationToken, bucketPrefix)
	}

	contents, commonPrefixes := buildListContents(result.Objects, result.CommonPrefixes, bucketPrefix)

	xmlResult := xmlListResultV1{
		Xmlns:          "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:           bucket,
		Prefix:         userPrefix,
		Marker:         marker,
		NextMarker:     nextMarker,
		Delimiter:      delimiter,
		MaxKeys:        maxKeys,
		IsTruncated:    result.IsTruncated,
		Contents:       contents,
		CommonPrefixes: commonPrefixes,
	}

	if err := writeXML(w, http.StatusOK, xmlResult); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode list response: %w", err)
	}

	return http.StatusOK, nil
}

// handleListObjectsV2 processes GET requests at the bucket level, returning an
// S3-compatible ListObjectsV2 XML response. Internally prefixes queries with
// the bucket name and strips the prefix from results before returning to clients.
func (s *Server) handleListObjectsV2(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket string) (int, error) {
	bucketPrefix := bucket + "/"

	userPrefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	continuationToken := r.URL.Query().Get("continuation-token")
	maxKeys := parseQueryInt(r, "max-keys", 1000, 1000)

	// Prepend bucket prefix to internal query parameters
	internalPrefix := bucketPrefix + userPrefix

	startAfter := r.URL.Query().Get("start-after")
	if continuationToken != "" {
		startAfter = continuationToken
	}
	if startAfter != "" {
		startAfter = bucketPrefix + startAfter
	}

	result, err := s.Manager.ListObjects(ctx, internalPrefix, delimiter, startAfter, maxKeys)
	if err != nil {
		return writeStorageError(w, err, "Failed to list objects"), err
	}

	// Strip bucket prefix from NextContinuationToken
	nextToken := result.NextContinuationToken
	if nextToken != "" {
		nextToken = strings.TrimPrefix(nextToken, bucketPrefix)
	}

	contents, commonPrefixes := buildListContents(result.Objects, result.CommonPrefixes, bucketPrefix)

	xmlResult := xmlListResultV2{
		Xmlns:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:                  bucket,
		Prefix:                userPrefix,
		Delimiter:             delimiter,
		MaxKeys:               maxKeys,
		KeyCount:              result.KeyCount,
		IsTruncated:           result.IsTruncated,
		NextContinuationToken: nextToken,
		Contents:              contents,
		CommonPrefixes:        commonPrefixes,
	}

	if continuationToken != "" {
		xmlResult.ContinuationToken = continuationToken
	}

	if err := writeXML(w, http.StatusOK, xmlResult); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode list response: %w", err)
	}

	return http.StatusOK, nil
}
