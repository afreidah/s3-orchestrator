// -------------------------------------------------------------------------------
// S3 API - Action Classification
//
// Author: Alex Freidah
//
// Names the S3 operation a request asks for, from its method, query string and
// path alone. Classification is separated from dispatch because the action has
// to be known before a handler runs: an authorization check has nowhere to sit
// if the operation is only named on the way out, once the response is written.
//
// Every function here is pure. The action set becomes a table a test can walk
// rather than something discovered by driving requests through handlers.
// -------------------------------------------------------------------------------

package s3api

import (
	"net/http"
	"net/url"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Action is the S3 operation a request names. The string form is what metrics
// and the audit log are keyed on, so the constants below are the operation
// names those consumers already carry.
type Action string

// The bucket-level operations, reached when a request names no object key.
const (
	ActionHeadBucket          Action = "HeadBucket"
	ActionGetBucketVersioning Action = "GetBucketVersioning"
	ActionGetBucketLocation   Action = "GetBucketLocation"
	ActionListMultipartUpload Action = "ListMultipartUploads"
	ActionListObjectsV1       Action = "ListObjectsV1"
	ActionListObjectsV2       Action = "ListObjectsV2"
	ActionDeleteObjects       Action = "DeleteObjects"
)

// The object-level operations, including the multipart and tagging
// subresources.
const (
	ActionGetObject               Action = "GetObject"
	ActionHeadObject              Action = "HeadObject"
	ActionPutObject               Action = "PutObject"
	ActionCopyObject              Action = "CopyObject"
	ActionDeleteObject            Action = "DeleteObject"
	ActionCreateMultipartUpload   Action = "CreateMultipartUpload"
	ActionUploadPart              Action = "UploadPart"
	ActionUploadPartCopy          Action = "UploadPartCopy"
	ActionCompleteMultipartUpload Action = "CompleteMultipartUpload"
	ActionAbortMultipartUpload    Action = "AbortMultipartUpload"
	ActionListParts               Action = "ListParts"
	ActionGetObjectTagging        Action = "GetObjectTagging"
	ActionPutObjectTagging        Action = "PutObjectTagging"
	ActionDeleteObjectTagging     Action = "DeleteObjectTagging"
)

// ActionUnsupportedSubresource names a request whose query selects a
// subresource this server does not implement, and ActionUnknown one whose
// method and query match no supported combination at all. They are different
// answers: the first is a 501 the caller reaches deliberately, the second the
// 405 that falls out of a method the operation does not accept.
const (
	ActionUnsupportedSubresource Action = "UnsupportedSubresource"
	ActionUnknown                Action = ""
)

// -------------------------------------------------------------------------
// CLASSIFICATION
// -------------------------------------------------------------------------

// Classify names the operation a request asks for. An empty key selects the
// bucket vocabulary, matching how the router splits.
//
// The copy-source header participates because S3 distinguishes PutObject from
// CopyObject by its presence alone, and the two are different operations to
// authorize: one writes bytes the caller supplied, the other reads an object
// the caller named.
func Classify(r *http.Request, key string) Action {
	query := r.URL.Query()
	if key == "" {
		return classifyBucket(r.Method, query)
	}
	return classifyObject(r.Method, query, r.Header.Get(headerCopySource) != "")
}

// classifyBucket names a bucket-level operation.
func classifyBucket(method string, query url.Values) Action {
	if _, unsupported := unsupportedQuery(query, supportedBucketQueryKeys, supportedBucketQueryPrefixes); unsupported {
		return ActionUnsupportedSubresource
	}

	_, hasDelete := query["delete"]
	_, hasLocation := query["location"]
	_, hasUploads := query["uploads"]
	_, hasVersioning := query["versioning"]

	switch {
	case method == http.MethodHead:
		return ActionHeadBucket
	case method == http.MethodGet && hasVersioning:
		return ActionGetBucketVersioning
	case method == http.MethodGet && hasUploads:
		return ActionListMultipartUpload
	case method == http.MethodGet && hasLocation:
		return ActionGetBucketLocation
	case method == http.MethodGet && query.Get("list-type") == "2":
		return ActionListObjectsV2
	case method == http.MethodGet:
		return ActionListObjectsV1
	case method == http.MethodPost && hasDelete:
		return ActionDeleteObjects
	}
	return ActionUnknown
}

// classifyObject names an object-level operation, splitting on the multipart
// and tagging subresources the way the router does.
//
// Tagging is tested ahead of the multipart split because a tagging request
// carries neither uploads nor uploadId, so it would otherwise classify as the
// plain object operation its method implies.
func classifyObject(method string, query url.Values, hasCopySource bool) Action {
	if _, unsupported := unsupportedQuery(query, supportedObjectQueryKeys, supportedObjectQueryPrefixes); unsupported {
		return ActionUnsupportedSubresource
	}
	if _, hasTagging := query["tagging"]; hasTagging {
		return classifyTagging(method)
	}

	_, hasUploads := query["uploads"]
	switch {
	case hasUploads && method == http.MethodPost:
		return ActionCreateMultipartUpload
	case query.Get("uploadId") != "":
		return classifyMultipart(method, hasCopySource)
	}
	return classifyPlainObject(method, hasCopySource)
}

// classifyTagging names one of the three ?tagging subresource operations.
func classifyTagging(method string) Action {
	switch method {
	case http.MethodGet:
		return ActionGetObjectTagging
	case http.MethodPut:
		return ActionPutObjectTagging
	case http.MethodDelete:
		return ActionDeleteObjectTagging
	}
	return ActionUnknown
}

// classifyMultipart names a per-uploadId multipart operation.
func classifyMultipart(method string, hasCopySource bool) Action {
	switch method {
	case http.MethodPut:
		if hasCopySource {
			return ActionUploadPartCopy
		}
		return ActionUploadPart
	case http.MethodPost:
		return ActionCompleteMultipartUpload
	case http.MethodDelete:
		return ActionAbortMultipartUpload
	case http.MethodGet:
		return ActionListParts
	}
	return ActionUnknown
}

// classifyPlainObject names a non-multipart, non-tagging object operation.
func classifyPlainObject(method string, hasCopySource bool) Action {
	switch method {
	case http.MethodPut:
		if hasCopySource {
			return ActionCopyObject
		}
		return ActionPutObject
	case http.MethodGet:
		return ActionGetObject
	case http.MethodHead:
		return ActionHeadObject
	case http.MethodDelete:
		return ActionDeleteObject
	}
	return ActionUnknown
}
