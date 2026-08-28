// -------------------------------------------------------------------------------
// Object Tagging Handlers - PutObjectTagging, GetObjectTagging, DeleteObjectTagging
//
// Author: Alex Freidah
//
// The three `?tagging` subresource operations on an object. Tags describe the
// object rather than any one copy of it, so these never reach a backend: the
// set lives in the metadata store and every replica shares it.
//
// Validation, the key lock, and the refusal of a key that holds no copies all
// live in the store layer. This file owns the wire format and the translation
// of those refusals into S3 error codes.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// WIRE FORMAT
// -------------------------------------------------------------------------

// maxTaggingBody caps the Tagging request body. Ten tags at the maximum key
// and value lengths is roughly 4 KB before SDK indentation, so this sits far
// above any legal request while still bounding what an attacker can send.
const maxTaggingBody = 64 << 10

// taggingDocument is the Tagging XML document carried by PutObjectTagging
// requests and GetObjectTagging responses.
//
// XMLName pins the root element so a document rooted at anything else is
// refused rather than silently decoding into an empty tag set.
type taggingDocument struct {
	XMLName struct{} `xml:"Tagging"`
	TagSet  tagSet   `xml:"TagSet"`
}

// tagSet wraps the repeated Tag elements. The extra level of nesting is the
// S3 schema's, not ours.
type tagSet struct {
	Tags []tagEntry `xml:"Tag"`
}

// tagEntry is one key/value pair. Both are case sensitive.
type tagEntry struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// toCoreTags converts a decoded document into the canonical tag type.
func (d *taggingDocument) toCoreTags() []core.Tag {
	out := make([]core.Tag, len(d.TagSet.Tags))
	for i, t := range d.TagSet.Tags {
		out[i] = core.Tag{Key: t.Key, Value: t.Value}
	}
	return out
}

// taggingDocumentFrom builds the response document for a stored tag set.
//
// Sorted by key so the response is byte-identical run to run. The store
// already orders its rows, but sorting here means the wire format does not
// depend on that promise holding in both engines.
func taggingDocumentFrom(tags []core.Tag) *taggingDocument {
	entries := make([]tagEntry, len(tags))
	for i, t := range tags {
		entries[i] = tagEntry{Key: t.Key, Value: t.Value}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return &taggingDocument{TagSet: tagSet{Tags: entries}}
}

// -------------------------------------------------------------------------
// ERROR MAPPING
// -------------------------------------------------------------------------

// writeTaggingError renders a store-layer failure as the S3 error the spec
// names for it, falling back to the shared storage-error mapping.
//
// The validation sentinels are mapped here rather than being S3Error values in
// core because the message carries the offending measurement, which a shared
// error value would have to drop.
func writeTaggingError(w http.ResponseWriter, err error) int {
	switch {
	case errors.Is(err, core.ErrObjectNotFound):
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist")
		return http.StatusNotFound
	case errors.Is(err, core.ErrTooManyTags):
		writeS3Error(w, http.StatusBadRequest, "BadRequest", err.Error())
		return http.StatusBadRequest
	case errors.Is(err, core.ErrEmptyTagKey),
		errors.Is(err, core.ErrTagKeyTooLong),
		errors.Is(err, core.ErrTagValueTooLong),
		errors.Is(err, core.ErrDuplicateTagKey):
		writeS3Error(w, http.StatusBadRequest, "InvalidTag", err.Error())
		return http.StatusBadRequest
	}
	return writeStorageError(w, err, "Failed to process object tagging")
}

// -------------------------------------------------------------------------
// HANDLERS
// -------------------------------------------------------------------------

// handleGetObjectTagging returns the object's tag set as a Tagging document.
// An untagged object answers 200 with an empty TagSet rather than 404: the
// object exists and simply carries nothing.
func (s *Server) handleGetObjectTagging(ctx context.Context, w http.ResponseWriter, key string) (int, error) {
	tags, err := s.Objects.GetObjectTags(ctx, key)
	if err != nil {
		return writeTaggingError(w, err), err
	}
	if err := writeXML(w, http.StatusOK, taggingDocumentFrom(tags)); err != nil {
		s.logger().ErrorContext(ctx, "failed to write tagging response", "key", key, logfmt.Err(err))
		return http.StatusOK, err
	}
	return http.StatusOK, nil
}

// handlePutObjectTagging replaces the object's whole tag set with the one in
// the request body. An empty TagSet removes every tag, which the spec defines
// as the same outcome as DeleteObjectTagging.
func (s *Server) handlePutObjectTagging(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	var doc taggingDocument
	if status, err := decodeXMLBody(w, r, maxTaggingBody, &doc); err != nil {
		return status, fmt.Errorf("put object tagging: %w", err)
	}

	tags := doc.toCoreTags()
	if err := s.Objects.PutObjectTags(ctx, key, tags); err != nil {
		return writeTaggingError(w, err), err
	}

	audit.Log(ctx, "s3.PutObjectTagging",
		slog.String("key", key),
		slog.Int("tag_count", len(tags)),
	)
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleDeleteObjectTagging removes the object's whole tag set. Removing a set
// that is already empty succeeds: the object is there and still has no tags.
func (s *Server) handleDeleteObjectTagging(ctx context.Context, w http.ResponseWriter, key string) (int, error) {
	if err := s.Objects.DeleteObjectTags(ctx, key); err != nil {
		return writeTaggingError(w, err), err
	}

	audit.Log(ctx, "s3.DeleteObjectTagging", slog.String("key", key))
	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent, nil
}
