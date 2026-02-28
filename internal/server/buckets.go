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

package server

import (
	"encoding/xml"
	"net/http"
	"time"
)

// -------------------------------------------------------------------------
// XML RESPONSE TYPES
// -------------------------------------------------------------------------

type xmlListBucketsResult struct {
	XMLName xml.Name   `xml:"ListAllMyBucketsResult"`
	Xmlns   string     `xml:"xmlns,attr"`
	Owner   xmlOwner   `xml:"Owner"`
	Buckets xmlBuckets `xml:"Buckets"`
}

type xmlOwner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type xmlBuckets struct {
	Bucket []xmlBucket `xml:"Bucket"`
}

type xmlBucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type xmlLocationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
}

// -------------------------------------------------------------------------
// HANDLERS
// -------------------------------------------------------------------------

// handleListBuckets returns the single bucket that the authenticated
// credential has access to. Satisfies GET / (ListBuckets).
func (s *Server) handleListBuckets(w http.ResponseWriter, bucket string) (int, error) {
	result := xmlListBucketsResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: xmlOwner{
			ID:          "s3-orchestrator",
			DisplayName: "s3-orchestrator",
		},
		Buckets: xmlBuckets{
			Bucket: []xmlBucket{
				{
					Name:         bucket,
					CreationDate: time.Now().UTC().Format(time.RFC3339),
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
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleGetBucketLocation returns a canned LocationConstraint response.
// Satisfies GET /{bucket}?location.
func (s *Server) handleGetBucketLocation(w http.ResponseWriter) (int, error) {
	result := xmlLocationConstraint{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}
