// -------------------------------------------------------------------------------
// Object Identity Encoding Tests
//
// Author: Alex Freidah
//
// The distinction these conversions exist to preserve is unknown versus known
// empty: a NULL column means nothing has recorded the object's identity yet
// and a read has to ask a backend, while an empty content type or an empty
// metadata set are answers in themselves. Both engines share this code so they
// cannot disagree about which is which.
// -------------------------------------------------------------------------------

package core

import "testing"

// TestEncodeUserMetadata_NilIsUnknownEmptyIsAnAnswer pins the two states apart.
func TestEncodeUserMetadata_NilIsUnknownEmptyIsAnAnswer(t *testing.T) {
	t.Parallel()

	got, err := EncodeUserMetadata(nil)
	if err != nil {
		t.Fatalf("EncodeUserMetadata(nil): %v", err)
	}
	if got != nil {
		t.Errorf("encoded nil map = %q, want nil so the column stays NULL", got)
	}

	got, err = EncodeUserMetadata(map[string]string{})
	if err != nil {
		t.Fatalf("EncodeUserMetadata(empty): %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("encoded empty map = %q, want {}", got)
	}
}

// TestUserMetadata_RoundTrips covers the pairing the engines rely on.
func TestUserMetadata_RoundTrips(t *testing.T) {
	t.Parallel()
	in := map[string]string{"colour": "green", "owner": "team-a"}

	encoded, err := EncodeUserMetadata(in)
	if err != nil {
		t.Fatalf("EncodeUserMetadata: %v", err)
	}
	out, err := DecodeUserMetadata(encoded)
	if err != nil {
		t.Fatalf("DecodeUserMetadata: %v", err)
	}
	if len(out) != len(in) || out["colour"] != "green" || out["owner"] != "team-a" {
		t.Errorf("round trip = %v, want %v", out, in)
	}
}

// TestDecodeUserMetadata_EmptyColumnIsNil covers a NULL column, which is what
// every row written before identity was recorded holds.
func TestDecodeUserMetadata_EmptyColumnIsNil(t *testing.T) {
	t.Parallel()
	got, err := DecodeUserMetadata(nil)
	if err != nil {
		t.Fatalf("DecodeUserMetadata(nil): %v", err)
	}
	if got != nil {
		t.Errorf("decoded NULL = %v, want nil", got)
	}
}

// TestDecodeUserMetadata_MalformedErrors pins that a corrupt column is
// reported rather than silently read as an object with no metadata.
func TestDecodeUserMetadata_MalformedErrors(t *testing.T) {
	t.Parallel()
	if _, err := DecodeUserMetadata([]byte("{not json")); err == nil {
		t.Error("expected an error for a malformed metadata column")
	}
}

// TestIdentityFromColumns_AllNullIsNoIdentity is the case a read has to
// recognise: nothing was ever recorded, so the backend still has to be asked.
func TestIdentityFromColumns_AllNullIsNoIdentity(t *testing.T) {
	t.Parallel()
	id, err := IdentityFromColumns("", "", nil)
	if err != nil {
		t.Fatalf("IdentityFromColumns: %v", err)
	}
	if id != nil {
		t.Errorf("identity = %+v, want nil", id)
	}
	if id.Complete() {
		t.Error("nil identity reported complete")
	}
}

// TestIdentityFromColumns_ETagIsWhatMakesItComplete pins the rule the read
// path branches on: a row without an ETag cannot answer a HEAD, whatever else
// it carries.
func TestIdentityFromColumns_ETagIsWhatMakesItComplete(t *testing.T) {
	t.Parallel()

	id, err := IdentityFromColumns("", "text/plain", []byte(`{}`))
	if err != nil {
		t.Fatalf("IdentityFromColumns: %v", err)
	}
	if id == nil {
		t.Fatal("identity = nil, want one carrying the content type")
	}
	if id.Complete() {
		t.Error("identity without an ETag reported complete")
	}

	id, err = IdentityFromColumns(`"abc"`, "text/plain", []byte(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("IdentityFromColumns: %v", err)
	}
	if !id.Complete() {
		t.Error("identity with an ETag reported incomplete")
	}
	if id.UserMetadata["k"] != "v" {
		t.Errorf("metadata = %v, want the decoded column", id.UserMetadata)
	}
}

// TestIdentityFromColumns_MalformedMetadataErrors keeps a corrupt column from
// producing a half-built identity a read would then serve.
func TestIdentityFromColumns_MalformedMetadataErrors(t *testing.T) {
	t.Parallel()
	if _, err := IdentityFromColumns(`"abc"`, "", []byte("{oops")); err == nil {
		t.Error("expected an error for a malformed metadata column")
	}
}
