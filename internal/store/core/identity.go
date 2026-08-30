// -------------------------------------------------------------------------------
// Object Identity - Column Encoding
//
// Author: Alex Freidah
//
// The conversions between an *ObjectIdentity and the three nullable columns it
// occupies, shared by both engines so they cannot disagree about what a NULL
// means. User metadata is JSON in one column rather than a side table: it is
// read and written whole, and no query filters on a single key.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"
	"fmt"
)

// EncodeUserMetadata renders a user-metadata map for storage. A nil map
// encodes as nil so the column stays NULL - "never recorded" - while an empty
// map encodes as {}, which is the answer that an object carries none.
func EncodeUserMetadata(m map[string]string) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode user metadata: %w", err)
	}
	return b, nil
}

// DecodeUserMetadata reads a stored user-metadata column. A NULL or empty
// column yields a nil map, which is what an object whose metadata was never
// recorded has.
func DecodeUserMetadata(b []byte) (map[string]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode user metadata: %w", err)
	}
	return m, nil
}

// IdentityFromColumns assembles the identity a row carries, or nil when the
// row carries none. An ETag alone is enough to build one: it is the field a
// read cannot answer without, and the other two are legitimately absent on an
// object written with neither a content type nor metadata.
func IdentityFromColumns(etag, contentType string, userMetadata []byte) (*ObjectIdentity, error) {
	if etag == "" && contentType == "" && len(userMetadata) == 0 {
		return nil, nil
	}
	meta, err := DecodeUserMetadata(userMetadata)
	if err != nil {
		return nil, err
	}
	return &ObjectIdentity{ETag: etag, ContentType: contentType, UserMetadata: meta}, nil
}
