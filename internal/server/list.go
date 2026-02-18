// -------------------------------------------------------------------------------
// List Handler - S3 ListObjectsV2
//
// Author: Alex Freidah
//
// HTTP handler for the S3 ListObjectsV2 operation. Returns XML responses
// compatible with S3 clients, supporting prefix filtering, delimiter-based
// directory grouping, and pagination via continuation tokens. Translates
// between external user-facing keys and internal prefixed keys.
// -------------------------------------------------------------------------------

package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleListObjectsV2 processes GET requests at the bucket level, returning an
// S3-compatible ListObjectsV2 XML response. Internally prefixes queries with
// the bucket name and strips the prefix from results before returning to clients.
func (s *Server) handleListObjectsV2(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket string) (int, error) {
	bucketPrefix := bucket + "/"

	userPrefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	continuationToken := r.URL.Query().Get("continuation-token")
	maxKeysStr := r.URL.Query().Get("max-keys")

	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= 1000 {
			maxKeys = mk
		}
	}

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

	type xmlContent struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	}

	type xmlCommonPrefix struct {
		Prefix string `xml:"Prefix"`
	}

	type xmlListResult struct {
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

	// Strip bucket prefix from NextContinuationToken
	nextToken := result.NextContinuationToken
	if nextToken != "" {
		nextToken = strings.TrimPrefix(nextToken, bucketPrefix)
	}

	xmlResult := xmlListResult{
		Xmlns:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:                  bucket,
		Prefix:                userPrefix,
		Delimiter:             delimiter,
		MaxKeys:               maxKeys,
		KeyCount:              result.KeyCount,
		IsTruncated:           result.IsTruncated,
		NextContinuationToken: nextToken,
	}

	if continuationToken != "" {
		xmlResult.ContinuationToken = continuationToken
	}

	// Strip bucket prefix from each returned key
	for _, obj := range result.Objects {
		xmlResult.Contents = append(xmlResult.Contents, xmlContent{
			Key:          strings.TrimPrefix(obj.ObjectKey, bucketPrefix),
			Size:         obj.SizeBytes,
			LastModified: obj.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	// Strip bucket prefix from common prefixes
	for _, cp := range result.CommonPrefixes {
		xmlResult.CommonPrefixes = append(xmlResult.CommonPrefixes, xmlCommonPrefix{
			Prefix: strings.TrimPrefix(cp, bucketPrefix),
		})
	}

	if err := writeXML(w, http.StatusOK, xmlResult); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode list response: %w", err)
	}

	return http.StatusOK, nil
}
