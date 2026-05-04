// -------------------------------------------------------------------------------
// Bucket Handlers - HeadBucket, GetBucketLocation, ListBuckets
//
// Author: Alex Freidah
//
// Stub handlers for S3 bucket-level operations that most clients call before
// performing real work. All responses are answered from config alone with no
// backend calls. HeadBucket confirms a bucket exists, GetBucketLocation returns
// an empty location constraint, and ListBuckets returns the single bucket that
// the authenticated credential has access to.
// -------------------------------------------------------------------------------

package s3api

import (
	"encoding/xml"
	"net/http"
	"time"
)

// -------------------------------------------------------------------------
// XML RESPONSE TYPES
// -------------------------------------------------------------------------

// xmlListBucketsResult is the XML envelope returned by ListBuckets.
// Field tags must match the AWS S3 1996 XML schema exactly so v2 SDK
// clients can unmarshal the response.
type xmlListBucketsResult struct {
	XMLName xml.Name   `xml:"ListAllMyBucketsResult"`
	Xmlns   string     `xml:"xmlns,attr"`
	Owner   xmlOwner   `xml:"Owner"`
	Buckets xmlBuckets `xml:"Buckets"`
}

// xmlOwner is the bucket-owner block included in ListBuckets and
// individual Bucket envelopes. The orchestrator returns the configured
// virtual-bucket owner for both fields.
type xmlOwner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// xmlBuckets wraps the Bucket array in the ListBuckets response so the
// XML output matches S3's nested <Buckets><Bucket/></Buckets> shape.
type xmlBuckets struct {
	Bucket []xmlBucket `xml:"Bucket"`
}

// xmlBucket is one entry inside ListBuckets - the bucket name and its
// creation date in RFC 3339. The orchestrator currently surfaces only
// the configured virtual buckets; physical backends are not exposed.
type xmlBucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// xmlLocationConstraint is the response body for GetBucketLocation.
// The orchestrator returns an empty body (us-east-1 by S3 convention)
// since there is no per-bucket region to expose.
type xmlLocationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
}

// xmlVersioningConfiguration is the empty response body returned by
// GetBucketVersioning. Object versioning is not implemented; the empty
// envelope keeps clients that probe the endpoint happy without
// promising features the orchestrator does not provide.
type xmlVersioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
}

// -------------------------------------------------------------------------
// HANDLERS
// -------------------------------------------------------------------------

// handleListBuckets returns the single bucket that the authenticated
// credential has access to. Satisfies GET / (ListBuckets).
func (s *Server) handleListBuckets(w http.ResponseWriter, bucket string) (int, error) {
	result := xmlListBucketsResult{
		Xmlns: s3XMLNS,
		Owner: xmlOwner{
			ID:          "s3-orchestrator",
			DisplayName: "s3-orchestrator",
		},
		Buckets: xmlBuckets{
			Bucket: []xmlBucket{
				{
					Name:         bucket,
					CreationDate: s.startedAt.UTC().Format(time.RFC3339),
				},
			},
		},
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}

// handleHeadBucket returns 200 with an empty body to confirm the bucket
// exists. The bucket is guaranteed to exist because auth already resolved it.
func (s *Server) handleHeadBucket(w http.ResponseWriter) (int, error) {
	w.Header().Set(headerContentType, "application/xml")
	w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleGetBucketLocation returns a canned LocationConstraint response.
// Satisfies GET /{bucket}?location.
func (s *Server) handleGetBucketLocation(w http.ResponseWriter) (int, error) {
	result := xmlLocationConstraint{
		Xmlns: s3XMLNS,
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}

// handleGetBucketVersioning returns an empty VersioningConfiguration to
// indicate that versioning is not enabled. Many S3 client libraries probe
// this endpoint on connect and error out without a response.
func (s *Server) handleGetBucketVersioning(w http.ResponseWriter) (int, error) {
	result := xmlVersioningConfiguration{
		Xmlns: s3XMLNS,
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}
