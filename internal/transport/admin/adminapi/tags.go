// -------------------------------------------------------------------------------
// Admin API - Object Tag DTOs
//
// Author: Alex Freidah
//
// Wire types for one object's tag set, and the conversions to and from the
// store's tag type. The conversions live beside the types so the admin
// handler, the TUI and any other surface that edits a set share one mapping.
// -------------------------------------------------------------------------------

package adminapi

import "github.com/afreidah/s3-orchestrator/internal/store/core"

// ObjectTag is one key/value label on an object. Both are case sensitive.
type ObjectTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ObjectTagsResponse is one object's whole tag set, ordered by key. An object
// carrying none reports an empty list rather than null, so a caller can render
// the result without a nil check.
type ObjectTagsResponse struct {
	Tags []ObjectTag `json:"tags"`
}

// ObjectTagsRequest replaces an object's whole tag set. An empty list leaves
// the object untagged, matching what the S3 endpoint does with an empty
// TagSet.
type ObjectTagsRequest struct {
	Tags []ObjectTag `json:"tags"`
}

// ObjectTagsFrom converts a stored tag set to the wire shape, always returning
// a non-nil slice so an untagged object serialises as [] rather than null.
func ObjectTagsFrom(tags []core.Tag) []ObjectTag {
	out := make([]ObjectTag, len(tags))
	for i, t := range tags {
		out[i] = ObjectTag{Key: t.Key, Value: t.Value}
	}
	return out
}

// CoreTags converts the wire shape to the store's tag type.
func CoreTags(tags []ObjectTag) []core.Tag {
	out := make([]core.Tag, len(tags))
	for i, t := range tags {
		out[i] = core.Tag{Key: t.Key, Value: t.Value}
	}
	return out
}
